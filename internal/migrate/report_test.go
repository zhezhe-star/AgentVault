package migrate

import (
	"bytes"
	"strings"
	"testing"
)

func TestReportClassesAndNoPlaintext(t *testing.T) {
	found, err := NewScanner(fixtureHome(t)).Scan()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	WriteReport(&buf, found, []string{"ark/ark_api_key", "misc/ark_api_key_config"})
	out := buf.String()
	for _, class := range []string{"[proxy-attach]", "[run-inject]", "[compliant]"} {
		if !strings.Contains(out, class) {
			t.Fatalf("report missing section %s", class)
		}
	}
	for _, s := range secretExpr() {
		if strings.Contains(out, s) {
			t.Fatalf("report leaks plaintext %s", s)
		}
	}
	if !strings.Contains(out, "agentvault run --env") || !strings.Contains(out, "add-key") {
		t.Fatalf("report missing run-inject guidance")
	}
	if !strings.Contains(out, "ref:ark/ark_api_key") {
		t.Fatalf("report missing imported refs")
	}
}
