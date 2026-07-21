package main

import (
	"strings"
	"testing"
)

func TestRenderSystemdUnit(t *testing.T) {
	identity := serviceIdentity{
		HomeDir: "/home/deploy",
		UID:     1001,
		GID:     1002,
	}
	unit, err := renderSystemdUnit(identity, "/home/deploy/pm config/pm.yaml", "/home/deploy/pm%20.sock")
	if err != nil {
		t.Fatal(err)
	}
	checks := []string{
		`User=1001`,
		`Group=1002`,
		`WorkingDirectory=/home/deploy/pm\x20config`,
		`Environment="HOME=/home/deploy"`,
		`ExecStart="/usr/local/bin/pm" daemon -config "/home/deploy/pm config/pm.yaml"`,
		`ExecReload="/usr/local/bin/pm" -socket "/home/deploy/pm%%20.sock" reload`,
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	}
	for _, check := range checks {
		if !strings.Contains(unit, check) {
			t.Fatalf("unit does not contain %q:\n%s", check, unit)
		}
	}
}

func TestSystemdQuoteRejectsNewlines(t *testing.T) {
	if _, err := systemdQuote("unsafe\nvalue"); err == nil {
		t.Fatal("expected newline rejection")
	}
}

func TestSystemdPathRequiresAbsolutePath(t *testing.T) {
	if _, err := systemdPath("relative/path"); err == nil {
		t.Fatal("expected relative path rejection")
	}
}
