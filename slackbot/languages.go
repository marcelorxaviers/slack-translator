package slackbot

import "strings"

// FallbackLangCode is used for any channel member who hasn't set a
// preferred language yet with /idioma.
const FallbackLangCode = "en"

// languageNames maps a language code (as typed in /idioma) to the name we
// send the LLM in the translation prompt and show in the Slack UI. Add more
// entries here as your team needs them - no code changes elsewhere required.
var languageNames = map[string]string{
	"pt-br": "Brazilian Portuguese",
	"pt-pt": "European Portuguese",
	"es-mx": "Mexican Spanish",
	"es-es": "European Spanish",
	"es":    "Spanish",
	"en":    "English",
	"en-us": "English (US)",
	"en-gb": "English (UK)",
	"fr":    "French",
	"de":    "German",
	"it":    "Italian",
	"nl":    "Dutch",
	"pl":    "Polish",
	"ja":    "Japanese",
	"zh-cn": "Simplified Chinese",
}

// NormalizeLangCode lowercases and trims a user-supplied language code so
// "ES-MX", " es-mx ", "Es-Mx" all map to the same entry.
func NormalizeLangCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// LangName returns the display/prompt name for a code, or the code itself
// (uppercased) if we don't have a friendlier name registered.
func LangName(code string) string {
	code = NormalizeLangCode(code)
	if name, ok := languageNames[code]; ok {
		return name
	}
	return strings.ToUpper(code)
}

// IsKnownLang reports whether code is in our registry. /idioma rejects
// unknown codes so a typo doesn't silently create a language nobody reads.
func IsKnownLang(code string) bool {
	_, ok := languageNames[NormalizeLangCode(code)]
	return ok
}

// SupportedLangsHelp renders a short "code - Name" list for the /idioma
// help response when called with no arguments.
func SupportedLangsHelp() string {
	// Fixed order for a stable, readable help message.
	order := []string{
		"pt-br", "pt-pt", "es-mx", "es-es", "es",
		"en", "en-us", "en-gb", "fr", "de", "it", "nl", "pl", "ja", "zh-cn",
	}
	var b strings.Builder
	for _, code := range order {
		b.WriteString("`" + code + "` - " + languageNames[code] + "\n")
	}
	return b.String()
}
