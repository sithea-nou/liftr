// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizedEnvironmentAllowlistsPulumiAndGoPaths(t *testing.T) {
	t.Setenv("LIFTR_DATABASE_URL", "must-not-be-inherited")
	pulumiPath := filepath.Join(string(filepath.Separator), "runtime", "pulumi", "bin", "pulumi")
	goPath := filepath.Join(string(filepath.Separator), "runtime", "go", "bin", "go")
	environment := sanitizedEnvironment(pulumiPath, goPath, []string{"PULUMI_HOME=/private/home", "LIFTR_INPUT_FILE=/private/input"})
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	wantPath := filepath.Dir(goPath) + string(os.PathListSeparator) + filepath.Dir(pulumiPath)
	if values["PATH"] != wantPath {
		t.Fatalf("PATH = %q, want %q", values["PATH"], wantPath)
	}
	if values["HOME"] != "/private/home" || values["LIFTR_INPUT_FILE"] != "/private/input" {
		t.Fatal("required private environment values were not retained")
	}
	if values["GOPROXY"] != "off" || values["GOSUMDB"] != "off" || values["GOTOOLCHAIN"] != "local" {
		t.Fatal("offline Go runtime policy was not enforced")
	}
	if _, inherited := values["LIFTR_DATABASE_URL"]; inherited {
		t.Fatal("ambient Liftr environment was inherited")
	}
}
