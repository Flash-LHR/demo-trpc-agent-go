package main

import (
	"context"
	"strings"
	"testing"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	aguisse "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	trpcevent "trpc.group/trpc-go/trpc-agent-go/event"
	trpcmodel "trpc.group/trpc-go/trpc-agent-go/model"
	trpcsession "trpc.group/trpc-go/trpc-agent-go/session"
	trpcinmemory "trpc.group/trpc-go/trpc-agent-go/session/inmemory"
)

func TestPrepareSessionSeedsHistoryAndReturnsLatestUserMessage(t *testing.T) {
	app := &serverApp{
		sessionService: trpcinmemory.NewSessionService(),
		userID:         defaultUserID,
	}
	input := aguitypes.RunAgentInput{
		ThreadID: "thread-demo",
		RunID:    "run-demo",
		Messages: []aguitypes.Message{
			{ID: "msg-1", Role: aguitypes.RoleUser, Content: "2+2"},
			{ID: "msg-2", Role: aguitypes.RoleAssistant, Content: "4"},
			{ID: "msg-3", Role: aguitypes.RoleUser, Content: "3+5"},
		},
	}
	latestUser, err := app.prepareSession(context.Background(), input)
	require.NoError(t, err)
	assert.Equal(t, trpcmodel.RoleUser, latestUser.Role)
	assert.Equal(t, "3+5", latestUser.Content)
	sessionKey := trpcsession.Key{AppName: appName, UserID: defaultUserID, SessionID: input.ThreadID}
	sess, err := app.sessionService.GetSession(context.Background(), sessionKey)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, sess.Events, 2)
	assert.Equal(t, trpcmodel.RoleUser, sess.Events[0].Response.Choices[0].Message.Role)
	assert.Equal(t, "2+2", sess.Events[0].Response.Choices[0].Message.Content)
	assert.Equal(t, trpcmodel.RoleAssistant, sess.Events[1].Response.Choices[0].Message.Role)
	assert.Equal(t, "4", sess.Events[1].Response.Choices[0].Message.Content)
}

func TestPrepareSessionRequiresUserMessage(t *testing.T) {
	app := &serverApp{
		sessionService: trpcinmemory.NewSessionService(),
		userID:         defaultUserID,
	}
	_, err := app.prepareSession(context.Background(), aguitypes.RunAgentInput{
		ThreadID: "thread-demo",
		RunID:    "run-demo",
		Messages: []aguitypes.Message{
			{ID: "msg-1", Role: aguitypes.RoleAssistant, Content: "hello"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "user message")
}

func TestStreamAGUIEmitsToolAndTextEvents(t *testing.T) {
	app := &serverApp{sseWriter: aguisseWriter()}
	input := aguitypes.RunAgentInput{ThreadID: "thread-demo", RunID: "run-demo"}
	eventCh := make(chan *trpcevent.Event, 3)
	eventCh <- &trpcevent.Event{
		Response: &trpcmodel.Response{
			Choices: []trpcmodel.Choice{{
				Message: trpcmodel.Message{
					Role: trpcmodel.RoleAssistant,
					ToolCalls: []trpcmodel.ToolCall{{
						ID:   "tool-call-1",
						Type: "function",
						Function: trpcmodel.FunctionDefinitionParam{
							Name:      "calculator",
							Arguments: []byte(`{"a":2,"b":3,"operation":"add"}`),
						},
					}},
				},
			}},
		},
	}
	eventCh <- &trpcevent.Event{
		Response: &trpcmodel.Response{
			Choices: []trpcmodel.Choice{{
				Message: trpcmodel.Message{
					Role:    trpcmodel.RoleTool,
					ToolID:  "tool-call-1",
					Content: `{"result":5}`,
				},
			}},
		},
	}
	eventCh <- &trpcevent.Event{
		Response: &trpcmodel.Response{
			IsPartial: true,
			Choices: []trpcmodel.Choice{{
				Delta: trpcmodel.Message{
					Role:    trpcmodel.RoleAssistant,
					Content: "The answer is 5.",
				},
			}},
		},
	}
	close(eventCh)
	recorder := httptest.NewRecorder()
	err := app.streamAGUI(context.Background(), recorder, input, eventCh)
	require.NoError(t, err)
	decodedEvents := decodeSSEEvents(t, recorder.Body.String())
	require.Len(t, decodedEvents, 9)
	assert.IsType(t, &aguievents.RunStartedEvent{}, decodedEvents[0])
	assert.IsType(t, &aguievents.TextMessageStartEvent{}, decodedEvents[1])
	assert.IsType(t, &aguievents.ToolCallStartEvent{}, decodedEvents[2])
	assert.IsType(t, &aguievents.ToolCallArgsEvent{}, decodedEvents[3])
	assert.IsType(t, &aguievents.ToolCallEndEvent{}, decodedEvents[4])
	assert.IsType(t, &aguievents.ToolCallResultEvent{}, decodedEvents[5])
	assert.IsType(t, &aguievents.TextMessageContentEvent{}, decodedEvents[6])
	assert.IsType(t, &aguievents.TextMessageEndEvent{}, decodedEvents[7])
	assert.IsType(t, &aguievents.RunFinishedEvent{}, decodedEvents[8])
	contentEvent, ok := decodedEvents[6].(*aguievents.TextMessageContentEvent)
	require.True(t, ok)
	assert.Equal(t, "The answer is 5.", contentEvent.Delta)
}

func aguisseWriter() *aguisse.SSEWriter {
	return aguisse.NewSSEWriter()
}

func decodeSSEEvents(t *testing.T, body string) []aguievents.Event {
	t.Helper()
	rawFrames := strings.Split(strings.TrimSpace(body), "\n\n")
	events := make([]aguievents.Event, 0, len(rawFrames))
	for _, frame := range rawFrames {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var dataLines []string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			}
		}
		require.NotEmpty(t, dataLines)
		event, err := aguievents.EventFromJSON([]byte(strings.Join(dataLines, "\n")))
		require.NoError(t, err)
		events = append(events, event)
	}
	return events
}
