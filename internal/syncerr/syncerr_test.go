package syncerr

import (
	"errors"
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	raw := Encode(New(CodeGitAuth, StageFetch, "denied"))
	code, stage, msg := Decode(raw)
	if code != CodeGitAuth || stage != StageFetch || msg != "denied" {
		t.Fatalf("got %s %s %s", code, stage, msg)
	}
}

func TestDecodeLegacyPlaintext(t *testing.T) {
	code, stage, msg := Decode("compose blew up")
	if code != CodeUnknown || stage != StageUnknown || msg != "compose blew up" {
		t.Fatalf("got %s %s %q", code, stage, msg)
	}
}

func TestClassify(t *testing.T) {
	se := Classify(errors.New("prepare repo failed: auth"))
	if se.Code != CodeGitFetch && se.Code != CodeGitAuth {
		t.Fatalf("code=%s", se.Code)
	}
}
