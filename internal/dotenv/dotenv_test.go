package dotenv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name, in string
		want     map[string]string
	}{
		{"plain", "A=b\nC=d", map[string]string{"A": "b", "C": "d"}},
		{"crlf", "A=b\r\nC=d\r\n", map[string]string{"A": "b", "C": "d"}},
		{"double quoted", `A="hello world # not comment"`, map[string]string{"A": "hello world # not comment"}},
		{"escapes", `A="x\ny\"z\\"`, map[string]string{"A": "x\ny\"z\\"}},
		{"single literal", `A='a\nb $X'`, map[string]string{"A": `a\nb $X`}},
		{"comments and blanks", "# hi\n\nA=1 # trailing\n", map[string]string{"A": "1"}},
		{"export", "export A=1\nexport  B='2'", map[string]string{"A": "1", "B": "2"}},
		{"empty", "A=\nB=\"\"", map[string]string{"A": "", "B": ""}},
		{"hash inside unquoted", "A=pa#ss", map[string]string{"A": "pa#ss"}},
		{"equals in value", "A=x=y", map[string]string{"A": "x=y"}},
		{"last wins", "A=1\nA=2", map[string]string{"A": "2"}},
		{"bom", "\ufeffA=1", map[string]string{"A": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.in))
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"justtext", "=v", "A B=1", `A="open`, "A='open"} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
	// Errors must not leak the value.
	_, err := Parse([]byte(`A="s3cr3t`))
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error %v leaks value", err)
	}
}

func TestLookupAndKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("DB_PASSWORD=pw\nEMPTY=\nB=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := Lookup(p, "DB_PASSWORD"); err != nil || v != "pw" {
		t.Fatalf("Lookup = %q, %v", v, err)
	}
	// Late rotation: next read sees the new value.
	if err := os.WriteFile(p, []byte("DB_PASSWORD=new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := Lookup(p, "DB_PASSWORD"); v != "new" {
		t.Fatalf("rotated Lookup = %q", v)
	}
	for _, tc := range []struct{ path, key, want string }{
		{p, "NOPE", "has no NOPE entry"},
		{filepath.Join(dir, "missing.env"), "A", "cannot read"},
	} {
		if _, err := Lookup(tc.path, tc.key); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("Lookup(%s,%s) err = %v, want %q naming the file", tc.path, tc.key, err, tc.want)
		}
	}
	if err := os.WriteFile(p, []byte("E=\nA=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(p, "E"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty value err = %v", err)
	}
	keys, err := Keys(p)
	if err != nil || !reflect.DeepEqual(keys, []string{"A", "E"}) {
		t.Fatalf("Keys = %v, %v", keys, err)
	}
}
