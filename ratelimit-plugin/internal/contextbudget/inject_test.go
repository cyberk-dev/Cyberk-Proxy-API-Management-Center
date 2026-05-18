package contextbudget

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const testReminder = "[reminder body for tests]"

// containsReminderText reports whether a (decoded) JSON string contains the
// reminder body. We check via gjson which decodes JSON unicode escapes so the
// test reads back the model-visible content rather than the on-the-wire
// bytes; this keeps assertions stable regardless of which mutation path was
// used to inject (encoding/json HTML-escapes vs sjson raw set).
func containsReminderText(field gjson.Result) bool {
	return strings.Contains(field.String(), testReminder)
}

func TestInject_Claude_AppendsNewUserMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	out := InjectSystemReminder(body, ProtoClaude, testReminder)
	if string(out) == string(body) {
		t.Fatal("expected body to be mutated")
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after inject, got %d", len(msgs))
	}
	// Original first message must be byte-identical so prompt caches still hit.
	if msgs[0].Get("content").String() != "hello" {
		t.Errorf("first message was mutated: %+v", msgs[0])
	}
	if msgs[0].Get("content").Type != gjson.String {
		t.Errorf("first message content type changed: %v (cache-busting)", msgs[0].Get("content").Type)
	}
	// Appended message is user-role with the reminder.
	if msgs[1].Get("role").String() != "user" {
		t.Errorf("appended message role = %q, want user", msgs[1].Get("role").String())
	}
	if !containsReminderText(msgs[1].Get("content.0.text")) {
		t.Errorf("appended message missing reminder body: %v", msgs[1])
	}
}

func TestInject_Claude_PreservesArrayContent(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]}
	]}`)
	out := InjectSystemReminder(body, ProtoClaude, testReminder)
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	// Original message's content must remain a 1-element array.
	original := msgs[0].Get("content").Array()
	if len(original) != 1 || original[0].Get("text").String() != "hi" {
		t.Errorf("original message content mutated: %+v", msgs[0])
	}
}

func TestInject_Claude_AppendsRegardlessOfPriorRole(t *testing.T) {
	// A multi-turn conversation where the last message is assistant — we
	// still append a fresh user message rather than going looking for the
	// "last user message" to mutate.
	body := []byte(`{"messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"reply"}
	]}`)
	out := InjectSystemReminder(body, ProtoClaude, testReminder)
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[2].Get("role").String() != "user" {
		t.Errorf("trailing appended message should be user, got %q", msgs[2].Get("role").String())
	}
}

func TestInject_OpenAIChat_AppendsNewUserMessage(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"hello"}
	]}`)
	out := InjectSystemReminder(body, ProtoOpenAIChat, testReminder)
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after inject, got %d", len(msgs))
	}
	// Original user message untouched.
	if msgs[1].Get("content").String() != "hello" {
		t.Errorf("original user message mutated: %+v", msgs[1])
	}
	if msgs[2].Get("role").String() != "user" {
		t.Errorf("appended role = %q, want user", msgs[2].Get("role").String())
	}
	if !containsReminderText(msgs[2].Get("content")) {
		t.Errorf("appended message missing reminder body: %v", msgs[2])
	}
}

func TestInject_OpenAIResponses_StringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":"hello"}`)
	out := InjectSystemReminder(body, ProtoOpenAIResponses, testReminder)
	input := gjson.GetBytes(out, "input")
	if !input.IsArray() {
		t.Fatalf("input should be promoted to array, got %s", input.Type)
	}
	arr := input.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 items after promotion, got %d", len(arr))
	}
	if arr[0].Get("content.0.text").String() != "hello" {
		t.Errorf("first item lost original prompt: %v", arr[0])
	}
	if !containsReminderText(arr[1].Get("content.0.text")) {
		t.Errorf("second item missing reminder body: %v", arr[1])
	}
}

func TestInject_OpenAIResponses_ArrayInput(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"system","content":[{"type":"input_text","text":"sys"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","name":"f","arguments":"{}"}
	]}`)
	out := InjectSystemReminder(body, ProtoOpenAIResponses, testReminder)
	arr := gjson.GetBytes(out, "input").Array()
	if len(arr) != 4 {
		t.Fatalf("expected 4 items after append, got %d", len(arr))
	}
	// Original user item untouched.
	if arr[1].Get("content.0.text").String() != "hi" {
		t.Errorf("existing user item was mutated: %+v", arr[1])
	}
	// Trailing function_call also untouched.
	if arr[2].Get("type").String() != "function_call" {
		t.Errorf("existing function_call disturbed: %+v", arr[2])
	}
	// Appended item is a fresh user message item carrying the reminder.
	if arr[3].Get("type").String() != "message" {
		t.Errorf("appended item type = %q, want message", arr[3].Get("type").String())
	}
	if arr[3].Get("role").String() != "user" {
		t.Errorf("appended item role = %q, want user", arr[3].Get("role").String())
	}
	if !containsReminderText(arr[3].Get("content.0.text")) {
		t.Errorf("appended item missing reminder body: %v", arr[3])
	}
}

func TestInject_Gemini_AppendsNewUserContent(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"first"}]},
		{"role":"model","parts":[{"text":"reply"}]}
	]}`)
	out := InjectSystemReminder(body, ProtoGemini, testReminder)
	contents := gjson.GetBytes(out, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("expected 3 contents entries, got %d", len(contents))
	}
	// Existing entries untouched.
	if contents[0].Get("parts.0.text").String() != "first" {
		t.Errorf("first content mutated: %v", contents[0])
	}
	if contents[1].Get("role").String() != "model" {
		t.Errorf("model entry mutated: %v", contents[1])
	}
	// Appended is a user content with the reminder text.
	if contents[2].Get("role").String() != "user" {
		t.Errorf("appended content role = %q, want user", contents[2].Get("role").String())
	}
	if !containsReminderText(contents[2].Get("parts.0.text")) {
		t.Errorf("appended content missing reminder body: %v", contents[2])
	}
}

func TestInject_Idempotent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	out := InjectSystemReminder(body, ProtoClaude, testReminder)
	out2 := InjectSystemReminder(out, ProtoClaude, testReminder)
	if string(out) != string(out2) {
		t.Error("second inject should be a no-op")
	}
}

func TestInject_EmptyReminderNoChange(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	out := InjectSystemReminder(body, ProtoClaude, "")
	if string(out) != string(body) {
		t.Error("empty reminder should pass through")
	}
}

func TestInject_UnknownProtocol(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	out := InjectSystemReminder(body, ProtoUnknown, testReminder)
	if string(out) != string(body) {
		t.Error("unknown protocol should pass through")
	}
}

func TestInject_NoMessagesArray(t *testing.T) {
	// Missing top-level messages: sjson can synthesize one via the array
	// append syntax. The behavior we want is "best effort" — we don't
	// strictly require append to fail, but if it does succeed, the
	// reminder must show up.
	body := []byte(`{"model":"claude-sonnet-4-6"}`)
	out := InjectSystemReminder(body, ProtoClaude, testReminder)
	// Best-effort: if a messages array was synthesized, it must contain
	// exactly one user-authored entry with the reminder.
	if msgs := gjson.GetBytes(out, "messages"); msgs.IsArray() {
		arr := msgs.Array()
		if len(arr) != 1 || arr[0].Get("role").String() != "user" {
			t.Errorf("synthesized messages unexpected shape: %v", msgs)
		}
	}
}
