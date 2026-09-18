package tui

import (
	"bytes"
	"testing"
)

type recordingSink struct {
	texts []string
}

func (s *recordingSink) Append(_, text string) {
	s.texts = append(s.texts, text)
}

func TestPrivateOutputIsVisibleButNotRecorded(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sink := &recordingSink{}
	out := NewOutputTo(&stdout, &stderr)
	out.Record(sink)

	out.Plain("open this URL")
	out.Private("https://example.test/oauth?state=one-time-secret")

	if got := stdout.String(); !bytes.Contains([]byte(got), []byte("one-time-secret")) {
		t.Errorf("private output was not shown in terminal output: %q", got)
	}
	if len(sink.texts) != 1 || sink.texts[0] != "open this URL" {
		t.Errorf("recorded messages = %#v; private output must be excluded", sink.texts)
	}
}
