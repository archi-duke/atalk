package hook

import (
	"encoding/json"
	"testing"
)

func TestIsSendCommand(t *testing.T) {
	cases := map[string]bool{
		`cd /c/Works/atalk; ./atalk.exe send - --type result --reply 1 <<'EOF'`: true,
		`atalk send "안녕"`:                    true,
		`"C:\Works\atalk\atalk.exe" send hi`: true,
		`atalk --as claude-code_x send hi`:   true,
		`atalk unread --ack`:                 false,
		`./atalk.exe log 12 done`:            false,
		`git send-email`:                     false,
		`echo send; atalk messages`:          false,
	}
	for cmd, want := range cases {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		if got := isSendCommand(Input{ToolInput: b}); got != want {
			t.Errorf("%q: got %v want %v", cmd, got, want)
		}
	}
}
