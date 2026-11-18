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

// knownPlaceholderMismatches have a different number of placeholders than the
// English text, so fixing them means rewording the sentence. Remove an entry
// once a translator has fixed it.
var knownPlaceholderMismatches = map[string]bool{
	"cs.toml/generateDalleHelpMessage":    true,
	"fi.toml/generateDalleHelpMessage":    true,
	"pl.toml/generateDalleHelpMessage":    true,
	"it.toml/inlineSendBalanceLowMessage": true,
	"tr.toml/advancedMessage":             true,
}

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
