package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type noopFlusher struct {
	count int
}

func (n *noopFlusher) Flush() {
	n.count++
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []Event {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	res := make([]Event, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var event Event
		require.NoError(t, json.Unmarshal(line, &event))
		res = append(res, event)
	}
	return res
}

func TestNewBufferWriterWriteSequence(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	flusher := &noopFlusher{}
	writer := NewBufferWriter(buf, flusher, "msg-1")

	require.NoError(t, writer.AppendAnswer("你好"))
	require.NoError(t, writer.AppendReasoning("先分析"))

	envelopes := decodeLines(t, buf)
	require.Len(t, envelopes, 2)
	assert.Equal(t, 2, flusher.count)
	assert.Equal(t, 1, int(envelopes[0].Sequence))
	assert.Equal(t, int64(2), envelopes[1].Sequence)
	assert.Equal(t, "msg-1", envelopes[0].MessageID)
	assert.Equal(t, EventTypeMessage, envelopes[0].Type)
	assert.Equal(t, EventActionAppend, envelopes[0].Action)
	assert.Equal(t, "你好", envelopes[0].Data)
	assert.Equal(t, EventTypeReasoning, envelopes[1].Type)
	assert.Equal(t, EventActionAppend, envelopes[1].Action)
	assert.Equal(t, "先分析", envelopes[1].Data)
}

func TestWriterEmitAndOptions(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	writer := NewBufferWriter(buf, nil, "msg-2")

	err := writer.Emit(EventTypeMessage, EventActionEnd,
		WithContent("最终答案"),
	)
	require.NoError(t, err)

	envelopes := decodeLines(t, buf)
	require.Len(t, envelopes, 1)
	assert.Equal(t, EventTypeMessage, envelopes[0].Type)
	assert.Equal(t, EventActionEnd, envelopes[0].Action)
	assert.Equal(t, "最终答案", envelopes[0].Data)
	require.NotNil(t, envelopes[0].Usage)
	assert.Equal(t, int32(30), envelopes[0].Usage.TotalTokens)
}

func TestWriterSupportsSSEFormat(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	writer := NewBufferWriter(buf, nil, "msg-sse").WithFormat(FormatSSE)

	err := writer.AppendAnswer("hello")
	require.NoError(t, err)

	output := buf.String()
	assert.True(t, strings.Contains(output, "id: 1\n"))
	assert.True(t, strings.Contains(output, "event: message\n"))
	assert.True(t, strings.Contains(output, "data: {"))
	assert.True(t, strings.HasSuffix(output, "\n\n"))
	assert.True(t, strings.Contains(output, `"data":"hello"`))
}

func TestWriterRejectsUnsupportedFormat(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	writer := NewBufferWriter(buf, nil, "msg-bad").WithFormat(Format("yaml"))

	err := writer.AppendAnswer("hello")
	assert.EqualError(t, err, "unsupported stream format: yaml")
}

func TestMarshalEventUsesNDJSONByDefault(t *testing.T) {
	writer := New("msg-http")
	payload, err := writer.marshalEvent(Event{Type: EventTypeStatus})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(payload), "\n"))
	assert.Contains(t, string(payload), `"type":"status"`)
}

func TestMarshalEventUsesSSEFormat(t *testing.T) {
	writer := New("msg-http").WithFormat(FormatSSE)
	payload, err := writer.marshalEvent(Event{Type: EventTypeMessage, Sequence: 7, Data: "hello"})
	require.NoError(t, err)
	assert.Contains(t, string(payload), "id: 7\n")
	assert.Contains(t, string(payload), "event: message\n")
	assert.Contains(t, string(payload), `"data":"hello"`)
	assert.True(t, strings.HasSuffix(string(payload), "\n\n"))
}

func TestEventBuilderAllowsOverrideMessageID(t *testing.T) {
	writer := New("msg-default")
	event := writer.Event(EventTypeMessage, EventActionReplace,
		WithMessageID("msg-custom"),
		WithContent("覆盖内容"),
	)

	assert.Equal(t, EventTypeMessage, event.Type)
	assert.Equal(t, EventActionReplace, event.Action)
	assert.Equal(t, "msg-custom", event.MessageID)
	assert.Equal(t, "覆盖内容", event.Data)
	assert.Zero(t, event.Sequence)
}

func TestNewUsageAndWriteWithoutOutput(t *testing.T) {

	usage := &Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	require.NotNil(t, usage)
	assert.Equal(t, int32(3), usage.TotalTokens)

	writer := New("msg-empty")
	err := writer.Write(Event{Type: EventTypeStatus})
	assert.EqualError(t, err, "writer output is nil")

	err = NewBufferWriter(bytes.NewBuffer(nil), nil, "msg-err").Emit(EventTypeError, "", WithError(errors.New("boom")))
	require.NoError(t, err)
}
