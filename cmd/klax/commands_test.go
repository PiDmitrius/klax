package main

import "testing"

func TestNormalizeCommandGroupAliases(t *testing.T) {
	tests := []struct {
		inCmd   string
		wantCmd string
		wantArg string
	}{
		{inCmd: "/group_on", wantCmd: "/groups", wantArg: "on"},
		{inCmd: "/group_off", wantCmd: "/groups", wantArg: "off"},
		{inCmd: "/groups_on", wantCmd: "/groups", wantArg: "on"},
		{inCmd: "/groups_off", wantCmd: "/groups", wantArg: "off"},
	}

	for _, tt := range tests {
		gotCmd, gotArgs := normalizeCommand(tt.inCmd, nil)
		if gotCmd != tt.wantCmd {
			t.Fatalf("%s normalized cmd = %q, want %q", tt.inCmd, gotCmd, tt.wantCmd)
		}
		if len(gotArgs) != 1 || gotArgs[0] != tt.wantArg {
			t.Fatalf("%s normalized args = %v, want [%q]", tt.inCmd, gotArgs, tt.wantArg)
		}
	}
}

func TestFormatAbortReply(t *testing.T) {
	tests := []struct {
		hasMessages bool
		want        string
	}{
		{hasMessages: false, want: "❌ Прервано."},
		{hasMessages: true, want: "❌ Прерваны все сообщения в сессии."},
	}

	for _, tt := range tests {
		if got := formatAbortReply(tt.hasMessages); got != tt.want {
			t.Fatalf("formatAbortReply(%v) = %q, want %q", tt.hasMessages, got, tt.want)
		}
	}
}
