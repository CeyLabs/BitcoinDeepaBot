package i18n

import (
	"io/fs"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/LightningTipBot/LightningTipBot/translations"
)

var fmtVerb = regexp.MustCompile(`%(?:\[\d+\])?[-+#0]*\d*(?:\.\d+)?[a-zA-Z]`)

// knownPlaceholderMismatches lists translations whose placeholders are
// knowingly out of step with English. Keep it empty: a mismatch shows users
// garbled text such as %!s(MISSING).
var knownPlaceholderMismatches = map[string]bool{}

// The code formats translated messages with fmt.Sprintf using the English
// argument list, so every translation must use the same verbs in the same
// order. A %d where the code passes a string shows users "%!d(string=...)".
func TestTranslationPlaceholdersMatchEnglish(t *testing.T) {
	load := func(name string) map[string]interface{} {
		buf, err := translations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]interface{}{}
		if err := toml.Unmarshal(buf, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	en := load("en.toml")
	files, err := fs.Glob(translations.FS, "*.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if name == "en.toml" {
			continue
		}
		for key, v := range load(name) {
			s, ok := v.(string)
			ref, refOK := en[key].(string)
			if !ok || !refOK || knownPlaceholderMismatches[name+"/"+key] {
				continue
			}
			if got, want := fmtVerb.FindAllString(s, -1), fmtVerb.FindAllString(ref, -1); !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: placeholders %s, English has %s", name, key, strings.Join(got, " "), strings.Join(want, " "))
			}
		}
	}
}
