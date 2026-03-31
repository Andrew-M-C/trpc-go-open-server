package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
)

const (
	headerSessionID = "X-Session-ID"
	headerUserID    = "X-User-ID"
)

type chatRequest struct {
	Message string `json:"message"`
}

type chatResponse struct {
	Reply     string `json:"reply"`
	SessionID string `json:"session_id"`
}

type chatErrorResponse struct {
	Error string `json:"error"`
}

// newChatMux exposes a minimal JSON API: one POST rounds the full agent loop
// (model + skills/tools) on the server; clients never handle tool_calls.
func newChatMux(r runner.Runner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		var body chatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeChatJSON(w, http.StatusBadRequest, chatErrorResponse{Error: "invalid json body"})
			return
		}
		if strings.TrimSpace(body.Message) == "" {
			writeChatJSON(w, http.StatusBadRequest, chatErrorResponse{Error: "message is required"})
			return
		}

		sessionID := strings.TrimSpace(req.Header.Get(headerSessionID))
		if sessionID == "" {
			sessionID = uuid.New().String()
		}
		userID := strings.TrimSpace(req.Header.Get(headerUserID))
		if userID == "" {
			userID = "default"
		}

		ch, err := r.Run(ctx, userID, sessionID, model.NewUserMessage(body.Message))
		if err != nil {
			writeChatJSON(w, http.StatusInternalServerError, chatErrorResponse{Error: err.Error()})
			return
		}

		var events []*event.Event
		for ev := range ch {
			if ev != nil {
				events = append(events, ev)
			}
		}
		reply := aggregateAssistantText(events)
		writeChatJSON(w, http.StatusOK, chatResponse{Reply: reply, SessionID: sessionID})
	})
	return mux
}

func writeChatJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func aggregateAssistantText(events []*event.Event) string {
	var deltaBuf strings.Builder
	var lastFinalAssistant string
	for _, evt := range events {
		if evt == nil || evt.Response == nil || len(evt.Response.Choices) == 0 {
			continue
		}
		rsp := evt.Response
		c := rsp.Choices[0]
		if c.Delta.Content != "" {
			deltaBuf.WriteString(c.Delta.Content)
		}
		// Multi-turn runs: first assistant stream may fill deltaBuf, then tools run;
		// the concluding assistant turn often appears only as Message.Content on
		// final chunks. IsFinalResponse is false for tool-call rows (see model.Response).
		if c.Message.Content != "" && rsp.IsFinalResponse() {
			lastFinalAssistant = c.Message.Content
		}
	}
	if lastFinalAssistant != "" {
		return lastFinalAssistant
	}
	return deltaBuf.String()
}
