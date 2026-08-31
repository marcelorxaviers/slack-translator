package slackbot

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// view identifies which version of a message is currently displayed in an
// ephemeral message: the translation, the raw original, or an English copy.
type view string

const (
	viewTranslated view = "translated"
	viewOriginal   view = "original"
	viewEnglish    view = "english"
)

// actionValue is encoded into each button's Value field so a later
// block_actions click can reconstruct exactly what to render, without
// needing external state beyond our own message store (see store.SaveMessage).
type actionValue struct {
	Channel    string `json:"channel"`
	TS         string `json:"ts"`          // original message timestamp (its unique ID in the channel)
	TargetLang string `json:"target_lang"` // recipient's preferred language code, e.g. "es-mx"
	TargetName string `json:"target_name"` // display name, e.g. "Mexican Spanish"
}

func encodeActionValue(v actionValue) string {
	b, _ := json.Marshal(v) // fixed shape, marshal error is not a real possibility here
	return string(b)
}

func decodeActionValue(raw string) (actionValue, error) {
	var v actionValue
	err := json.Unmarshal([]byte(raw), &v)
	return v, err
}

// shortModelName turns "meta-llama/llama-3.3-70b-instruct:free" into
// "llama-3.3-70b-instruct" for a compact "via <model>" footer.
func shortModelName(model string) string {
	name := strings.TrimSuffix(model, ":free")
	if idx := strings.LastIndex(name, "/"); idx != -1 {
		name = name[idx+1:]
	}
	return name
}

// BuildTranslationBlocks renders the ephemeral message shown to one
// recipient: a header naming the original author, the text for the requested
// view, a small context line noting the language pair and model (when
// relevant), and the three toggle buttons.
//
// currentView controls which button is visually de-emphasized (Slack block
// buttons don't support a true "pressed" state, so we approximate it by
// using the "primary" style only on the buttons NOT matching the current view).
func BuildTranslationBlocks(text string, currentView view, val actionValue, modelUsed string, fromCache bool, senderName string) []slack.Block {
	blocks := []slack.Block{}

	if currentView != viewOriginal && senderName != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*%s* said:", senderName), false, false),
		))
	}

	blocks = append(blocks,
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	)

	if currentView != viewOriginal && modelUsed != "" {
		cacheNote := ""
		if fromCache {
			cacheNote = " (cache)"
		}
		context := fmt.Sprintf("🌐 → %s · via `%s`%s", val.TargetName, shortModelName(modelUsed), cacheNote)
		if currentView == viewEnglish {
			context = fmt.Sprintf("🌐 → English · via `%s`%s", shortModelName(modelUsed), cacheNote)
		}
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", context, false, false),
		))
	}

	value := encodeActionValue(val)

	originalBtn := slack.NewButtonBlockElement("show_original", value,
		slack.NewTextBlockObject("plain_text", "View original", true, false))
	translatedBtn := slack.NewButtonBlockElement("show_translated", value,
		slack.NewTextBlockObject("plain_text", "View translated", true, false))
	englishBtn := slack.NewButtonBlockElement("show_english", value,
		slack.NewTextBlockObject("plain_text", "View in English", true, false))

	// Highlight the two views the user is NOT currently looking at, so the
	// buttons available make sense at a glance.
	switch currentView {
	case viewOriginal:
		translatedBtn.Style = "primary"
	case viewEnglish:
		originalBtn.Style = "primary"
	default: // viewTranslated
		originalBtn.Style = "primary"
	}

	actionBlock := slack.NewActionBlock("translation_actions", originalBtn, translatedBtn, englishBtn)
	blocks = append(blocks, actionBlock)

	return blocks
}
