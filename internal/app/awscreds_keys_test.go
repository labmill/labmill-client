package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
)

const typedKeyID = "AKIAABCDEFGHIJKLMNOP"

func typedRun(t *testing.T, answers []string, perr error) (res string, err error, sr *stdinRunner, p agent.Proposal, fields []PromptField) {
	t.Helper()
	awsTestFiles(t)
	sr = &stdinRunner{}
	h := remoteHarness(t, sr)
	h.env.SecretPrompt = func(ctx context.Context, title string, f []PromptField) ([]string, error) {
		fields = f
		return answers, perr
	}
	tool := h.tools["s3_credentials_to_cluster"]
	in := json.RawMessage(`{"cluster":"hpcone","enter_keys":true}`)
	p, err = tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Secret {
		t.Error("proposal must be Secret")
	}
	res, err = tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	return
}

func TestS3CredentialsTypedKeys(t *testing.T) {
	res, err, sr, p, fields := typedRun(t, []string{typedKeyID, "typedSECRET/value", "tok123", ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{res, p.Summary, p.Details} {
		if strings.Contains(s, "typedSECRET") || strings.Contains(s, typedKeyID) || strings.Contains(s, "tok123") {
			t.Errorf("value leaked: %q", s)
		}
	}
	if !strings.Contains(res, "AWS_PROFILE=labmill-manual") {
		t.Errorf("result: %q", res)
	}
	want := "[labmill-manual]\naws_access_key_id = " + typedKeyID + "\naws_secret_access_key = typedSECRET/value\naws_session_token = tok123\n"
	if sr.in != want {
		t.Errorf("stdin: %q", sr.in)
	}
	if strings.Contains(strings.Join(sr.got, " "), "typedSECRET") {
		t.Error("value in argv")
	}
	if len(fields) != 4 || fields[0].Secret || !fields[1].Secret || !fields[2].Secret {
		t.Errorf("fields: %+v", fields)
	}
}

func TestS3CredentialsTypedCancelAndBadInput(t *testing.T) {
	_, err, sr, _, _ := typedRun(t, nil, errors.New("closed"))
	if err == nil || !strings.Contains(err.Error(), "Cancelled") || sr.in != "" {
		t.Errorf("cancel: %v %q", err, sr.in)
	}
	for _, a := range [][]string{
		{"nope", "sEcReT99", "", ""},
		{typedKeyID, "", "", ""},
		{typedKeyID, "sEcReT99", "", "../x"},
	} {
		_, err, sr, _, _ := typedRun(t, a, nil)
		if err == nil || sr.in != "" || strings.Contains(err.Error(), "sEcReT99") {
			t.Errorf("%v: err=%v in=%q", a, err, sr.in)
		}
	}
}

func TestS3CredentialsNoPromptFallsBack(t *testing.T) {
	awsTestFiles(t)
	h := remoteHarness(t, &stdinRunner{})
	out, _ := h.run(t, "s3_credentials_to_cluster", map[string]any{"cluster": "hpcone", "enter_keys": true})
	if !strings.Contains(out, "aws configure") {
		t.Errorf("fallback: %q", out)
	}
}
