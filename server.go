package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	aguisse "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	trpcevent "trpc.group/trpc-go/trpc-agent-go/event"
	trpcmodel "trpc.group/trpc-go/trpc-agent-go/model"
	trpcopenai "trpc.group/trpc-go/trpc-agent-go/model/openai"
	trpcrunner "trpc.group/trpc-go/trpc-agent-go/runner"
	trpcsession "trpc.group/trpc-go/trpc-agent-go/session"
	trpcinmemory "trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	trpctool "trpc.group/trpc-go/trpc-agent-go/tool"
	trpcfunction "trpc.group/trpc-go/trpc-agent-go/tool/function"
)

const (
	appName          = "trpc-agent-demo"
	defaultAddr      = ":8080"
	defaultModelName = "deepseek-chat"
	defaultUserID    = "agui-demo"
	envServerAddr    = "SERVER_ADDR"
	envModelName     = "MODEL_NAME"
	envAPIKey        = "OPENAI_API_KEY"
	envBaseURL       = "OPENAI_BASE_URL"
)

type serverConfig struct {
	Addr      string
	ModelName string
	APIKey    string
	BaseURL   string
}

type serverApp struct {
	runner         trpcrunner.Runner
	sessionService trpcsession.Service
	sseWriter      *aguisse.SSEWriter
	userID         string
}

type calculatorArgs struct {
	A         float64 `json:"a" jsonschema:"description=The left operand,required"`
	B         float64 `json:"b" jsonschema:"description=The right operand,required"`
	Operation string  `json:"operation" jsonschema:"description=The operation name,enum=add,enum=subtract,enum=multiply,enum=divide,required"`
}

type calculatorResult struct {
	A         float64 `json:"a"`
	B         float64 `json:"b"`
	Operation string  `json:"operation"`
	Result    float64 `json:"result"`
}

func loadConfigFromEnv() (serverConfig, error) {
	cfg := serverConfig{
		Addr:      valueOrDefault(strings.TrimSpace(os.Getenv(envServerAddr)), defaultAddr),
		ModelName: valueOrDefault(strings.TrimSpace(os.Getenv(envModelName)), defaultModelName),
		APIKey:    strings.TrimSpace(os.Getenv(envAPIKey)),
		BaseURL:   strings.TrimSpace(os.Getenv(envBaseURL)),
	}
	if cfg.APIKey == "" {
		return serverConfig{}, fmt.Errorf("%s is required", envAPIKey)
	}
	return cfg, nil
}

func newServerApp(cfg serverConfig) (*serverApp, error) {
	sessionService := trpcinmemory.NewSessionService()
	modelOptions := []trpcopenai.Option{trpcopenai.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		modelOptions = append(modelOptions, trpcopenai.WithBaseURL(cfg.BaseURL))
	}
	modelInstance := trpcopenai.New(cfg.ModelName, modelOptions...)
	calculatorTool := trpcfunction.NewFunctionTool(
		calculate,
		trpcfunction.WithName("calculator"),
		trpcfunction.WithDescription("Perform arithmetic for the user. Always use this tool for calculations."),
	)
	generationConfig := trpcmodel.GenerationConfig{
		Stream:      true,
		MaxTokens:   intPtr(1024),
		Temperature: floatPtr(0.2),
	}
	agent := trpcagent.New(
		"calculator-assistant",
		trpcagent.WithModel(modelInstance),
		trpcagent.WithDescription("An AG-UI compatible assistant with a calculator tool."),
		trpcagent.WithInstruction("You are a concise math assistant. Use the calculator tool for arithmetic instead of mental math."),
		trpcagent.WithGenerationConfig(generationConfig),
		trpcagent.WithTools([]trpctool.Tool{calculatorTool}),
	)
	return &serverApp{
		runner:         trpcrunner.NewRunner(appName, agent, trpcrunner.WithSessionService(sessionService)),
		sessionService: sessionService,
		sseWriter:      aguisse.NewSSEWriter(),
		userID:         defaultUserID,
	}, nil
}

func (a *serverApp) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.withCORS(a.handleHealth))
	mux.HandleFunc("/agent", a.withCORS(a.handleAgent))
	return mux
}

func (a *serverApp) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (a *serverApp) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *serverApp) handleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var input aguitypes.RunAgentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	normalizeRunInput(&input)
	userMessage, err := a.prepareSession(r.Context(), input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	eventCh, err := a.runner.Run(r.Context(), a.userID, input.ThreadID, userMessage)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to run agent: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if err := a.streamAGUI(r.Context(), w, input, eventCh); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("stream AG-UI events failed: %v", err)
	}
}

func (a *serverApp) prepareSession(ctx context.Context, input aguitypes.RunAgentInput) (trpcmodel.Message, error) {
	lastUserIndex := -1
	var latestUser trpcmodel.Message
	for i, msg := range input.Messages {
		if msg.Role != aguitypes.RoleUser {
			continue
		}
		converted, ok, err := aguiMessageToTRPCMessage(msg)
		if err != nil {
			return trpcmodel.Message{}, err
		}
		if !ok {
			continue
		}
		lastUserIndex = i
		latestUser = converted
	}
	if lastUserIndex == -1 {
		return trpcmodel.Message{}, errors.New("at least one user message is required")
	}
	sessionKey := trpcsession.Key{AppName: appName, UserID: a.userID, SessionID: input.ThreadID}
	sess, err := a.sessionService.GetSession(ctx, sessionKey)
	if err != nil {
		return trpcmodel.Message{}, err
	}
	if sess == nil {
		sess, err = a.sessionService.CreateSession(ctx, sessionKey, trpcsession.StateMap{})
		if err != nil {
			return trpcmodel.Message{}, err
		}
	}
	if len(sess.Events) == 0 {
		baseTime := time.Now().Add(-time.Duration(lastUserIndex+1) * time.Millisecond)
		for i, msg := range input.Messages[:lastUserIndex] {
			seedEvent, ok, err := newSessionEventFromAGUIMessage(msg, baseTime.Add(time.Duration(i)*time.Millisecond))
			if err != nil {
				return trpcmodel.Message{}, err
			}
			if !ok {
				continue
			}
			if err := a.sessionService.AppendEvent(ctx, sess, seedEvent); err != nil {
				return trpcmodel.Message{}, err
			}
		}
	}
	if strings.TrimSpace(latestUser.Content) == "" && len(latestUser.ContentParts) == 0 {
		return trpcmodel.Message{}, errors.New("the latest user message must contain text content")
	}
	return latestUser, nil
}

func (a *serverApp) streamAGUI(
	ctx context.Context,
	w http.ResponseWriter,
	input aguitypes.RunAgentInput,
	eventCh <-chan *trpcevent.Event,
) error {
	threadID := input.ThreadID
	runID := input.RunID
	state := bridgeState{
		threadID:  threadID,
		runID:     runID,
		sseWriter: a.sseWriter,
		writer:    w,
	}
	if err := state.write(ctx, aguievents.NewRunStartedEvent(threadID, runID)); err != nil {
		return err
	}
	for evt := range eventCh {
		if evt == nil {
			continue
		}
		if evt.Error != nil {
			runErr := aguievents.NewRunErrorEvent(evt.Error.Message, aguievents.WithRunID(runID))
			if err := state.write(ctx, runErr); err != nil {
				return err
			}
			return nil
		}
		if err := state.handleToolCalls(ctx, evt); err != nil {
			return err
		}
		if err := state.handleToolResults(ctx, evt); err != nil {
			return err
		}
		if err := state.handleAssistantContent(ctx, evt); err != nil {
			return err
		}
	}
	if err := state.finishAssistantMessage(ctx); err != nil {
		return err
	}
	return state.write(ctx, aguievents.NewRunFinishedEvent(threadID, runID))
}

type bridgeState struct {
	threadID           string
	runID              string
	assistantMessageID string
	writer             http.ResponseWriter
	sseWriter          *aguisse.SSEWriter
}

func (s *bridgeState) handleToolCalls(ctx context.Context, evt *trpcevent.Event) error {
	if evt.Response == nil || len(evt.Response.Choices) == 0 {
		return nil
	}
	toolCalls := evt.Response.Choices[0].Message.ToolCalls
	if len(toolCalls) == 0 {
		return nil
	}
	if err := s.ensureAssistantMessage(ctx); err != nil {
		return err
	}
	for _, toolCall := range toolCalls {
		startEvent := aguievents.NewToolCallStartEvent(
			toolCall.ID,
			toolCall.Function.Name,
			aguievents.WithParentMessageID(s.assistantMessageID),
		)
		if err := s.write(ctx, startEvent); err != nil {
			return err
		}
		if len(toolCall.Function.Arguments) > 0 {
			if err := s.write(ctx, aguievents.NewToolCallArgsEvent(toolCall.ID, string(toolCall.Function.Arguments))); err != nil {
				return err
			}
		}
		if err := s.write(ctx, aguievents.NewToolCallEndEvent(toolCall.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *bridgeState) handleToolResults(ctx context.Context, evt *trpcevent.Event) error {
	if evt.Response == nil || len(evt.Response.Choices) == 0 {
		return nil
	}
	for _, choice := range evt.Response.Choices {
		if choice.Message.Role != trpcmodel.RoleTool || choice.Message.ToolID == "" {
			continue
		}
		toolMessageID := aguievents.GenerateMessageID()
		if err := s.write(ctx, aguievents.NewToolCallResultEvent(toolMessageID, choice.Message.ToolID, choice.Message.Content)); err != nil {
			return err
		}
	}
	return nil
}

func (s *bridgeState) handleAssistantContent(ctx context.Context, evt *trpcevent.Event) error {
	if evt.Response == nil || len(evt.Response.Choices) == 0 {
		return nil
	}
	for _, choice := range evt.Response.Choices {
		content := choice.Delta.Content
		if content == "" && !evt.Response.IsPartial && choice.Message.Role == trpcmodel.RoleAssistant {
			content = choice.Message.Content
		}
		if content == "" {
			continue
		}
		if err := s.ensureAssistantMessage(ctx); err != nil {
			return err
		}
		if err := s.write(ctx, aguievents.NewTextMessageContentEvent(s.assistantMessageID, content)); err != nil {
			return err
		}
	}
	return nil
}

func (s *bridgeState) ensureAssistantMessage(ctx context.Context) error {
	if s.assistantMessageID != "" {
		return nil
	}
	s.assistantMessageID = aguievents.GenerateMessageID()
	return s.write(ctx, aguievents.NewTextMessageStartEvent(s.assistantMessageID, aguievents.WithRole("assistant")))
}

func (s *bridgeState) finishAssistantMessage(ctx context.Context) error {
	if s.assistantMessageID == "" {
		return nil
	}
	return s.write(ctx, aguievents.NewTextMessageEndEvent(s.assistantMessageID))
}

func (s *bridgeState) write(ctx context.Context, event aguievents.Event) error {
	return s.sseWriter.WriteEvent(ctx, s.writer, event)
}

func newSessionEventFromAGUIMessage(msg aguitypes.Message, ts time.Time) (*trpcevent.Event, bool, error) {
	converted, ok, err := aguiMessageToTRPCMessage(msg)
	if err != nil || !ok {
		return nil, ok, err
	}
	response := &trpcmodel.Response{
		Done: false,
		Choices: []trpcmodel.Choice{{
			Index:   0,
			Message: converted,
		}},
	}
	event := trpcevent.NewResponseEvent("bootstrap", bootstrapAuthor(converted.Role), response)
	event.Timestamp = ts
	return event, true, nil
}

func bootstrapAuthor(role trpcmodel.Role) string {
	if role == trpcmodel.RoleUser {
		return "user"
	}
	return appName
}

func aguiMessageToTRPCMessage(msg aguitypes.Message) (trpcmodel.Message, bool, error) {
	switch msg.Role {
	case aguitypes.RoleUser:
		content, err := aguiMessageText(msg)
		if err != nil {
			return trpcmodel.Message{}, false, err
		}
		return trpcmodel.NewUserMessage(content), true, nil
	case aguitypes.RoleAssistant:
		content, err := aguiMessageText(msg)
		if err != nil {
			return trpcmodel.Message{}, false, err
		}
		assistantMsg := trpcmodel.NewAssistantMessage(content)
		if len(msg.ToolCalls) > 0 {
			assistantMsg.ToolCalls = make([]trpcmodel.ToolCall, 0, len(msg.ToolCalls))
			for _, toolCall := range msg.ToolCalls {
				assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, trpcmodel.ToolCall{
					ID:   toolCall.ID,
					Type: toolCall.Type,
					Function: trpcmodel.FunctionDefinitionParam{
						Name:      toolCall.Function.Name,
						Arguments: []byte(toolCall.Function.Arguments),
					},
				})
			}
		}
		return assistantMsg, true, nil
	case aguitypes.RoleTool:
		content, err := aguiMessageText(msg)
		if err != nil {
			return trpcmodel.Message{}, false, err
		}
		if msg.ToolCallID == "" {
			return trpcmodel.Message{}, false, errors.New("tool messages must include toolCallId")
		}
		return trpcmodel.NewToolMessage(msg.ToolCallID, "", content), true, nil
	case aguitypes.RoleSystem, aguitypes.RoleDeveloper:
		content, err := aguiMessageText(msg)
		if err != nil {
			return trpcmodel.Message{}, false, err
		}
		return trpcmodel.NewSystemMessage(content), true, nil
	default:
		return trpcmodel.Message{}, false, nil
	}
}

func aguiMessageText(msg aguitypes.Message) (string, error) {
	if content, ok := msg.ContentString(); ok {
		return content, nil
	}
	if parts, ok := msg.ContentInputContents(); ok {
		var builder strings.Builder
		for _, part := range parts {
			if part.Type != aguitypes.InputContentTypeText || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString(part.Text)
		}
		if builder.Len() > 0 {
			return builder.String(), nil
		}
	}
	if msg.Content == nil {
		return "", nil
	}
	raw, err := json.Marshal(msg.Content)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func normalizeRunInput(input *aguitypes.RunAgentInput) {
	if strings.TrimSpace(input.ThreadID) == "" {
		input.ThreadID = aguievents.GenerateThreadID()
	}
	if strings.TrimSpace(input.RunID) == "" {
		input.RunID = aguievents.GenerateRunID()
	}
}

func calculate(_ context.Context, args calculatorArgs) (calculatorResult, error) {
	result := calculatorResult{
		A:         args.A,
		B:         args.B,
		Operation: strings.ToLower(strings.TrimSpace(args.Operation)),
	}
	switch result.Operation {
	case "add", "+":
		result.Operation = "add"
		result.Result = args.A + args.B
	case "subtract", "sub", "-":
		result.Operation = "subtract"
		result.Result = args.A - args.B
	case "multiply", "mul", "*":
		result.Operation = "multiply"
		result.Result = args.A * args.B
	case "divide", "div", "/":
		if args.B == 0 {
			return calculatorResult{}, errors.New("division by zero is not allowed")
		}
		result.Operation = "divide"
		result.Result = args.A / args.B
	default:
		return calculatorResult{}, fmt.Errorf("unsupported operation %q", args.Operation)
	}
	return result, nil
}

func valueOrDefault(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func intPtr(v int) *int {
	return &v
}

func floatPtr(v float64) *float64 {
	return &v
}
