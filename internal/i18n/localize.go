package i18n

import (
	"github.com/LightningTipBot/LightningTipBot/translations"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	log "github.com/sirupsen/logrus"
	"golang.org/x/text/language"
)

var Bundle *i18n.Bundle

func init() {
	Bundle = RegisterLanguages()
}

// optionalLanguages are loaded if present. English is the fallback and must load.
var optionalLanguages = []string{
	"de.toml", "fi.toml", "it.toml", "es.toml", "nl.toml", "pl.toml", "fr.toml",
	"pt-br.toml", "tr.toml", "cs.toml", "id.toml", "ru.toml",
}

func RegisterLanguages() *i18n.Bundle {
	bundle := i18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	if err := loadEmbedded(bundle, "en.toml"); err != nil {
		panic(err)
	}
	for _, name := range optionalLanguages {
		if err := loadEmbedded(bundle, name); err != nil {
			log.Warnf("[i18n] Could not load %s: %v", name, err)
		}
	}
	return bundle
}

// loadEmbedded parses a message file compiled into the binary.
func loadEmbedded(bundle *i18n.Bundle, name string) error {
	buf, err := translations.FS.ReadFile(name)
	if err != nil {
		return err
	}
	_, err = bundle.ParseMessageFileBytes(buf, name)
	return err
}
func Translate(languageCode string, MessgeID string) string {
	str, err := i18n.NewLocalizer(Bundle, languageCode).Localize(&i18n.LocalizeConfig{MessageID: MessgeID})
	if err != nil {
		log.Warnf("Error translating message %s: %s", MessgeID, err)
	}
	return str
}
