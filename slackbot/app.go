package slackbot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/marceloribeiro/slack-translator/store"
	"github.com/marceloribeiro/slack-translator/translator"
)

// LangStore is the subset of store.Store the app needs for user
// preferences and original-message lookups (kept as an interface so app.go
// doesn't need to know about SQLite specifically).
type LangStore interface {
	SetUserLang(userID, lang string) error
	GetUserLang(userID string) (lang string, found bool, err error)
	SaveMessage(channel, ts, originalText string) error
	GetMessage(channel, ts string) (originalText string, found bool, err error)
}

// App wires together the Slack client, the translation pool, and storage.
type App struct {
	api    *slack.Client
	socket *socketmode.Client
	pool   *translator.Pool
	store  LangStore
	// BotUserID lets us ignore the bot's own messages if it ever posts
	// non-ephemeral content into a channel.
	botUserID string
}

func New(botToken, appToken string, pool *translator.Pool, st LangStore) (*App, error) {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))

	auth, err := api.AuthTest()
	if err != nil {
		return nil, fmt.Errorf("slack auth test failed (check SLACK_BOT_TOKEN): %w", err)
	}

	socket := socketmode.New(api)

	return &App{
		api:       api,
		socket:    socket,
		pool:      pool,
		store:     st,
		botUserID: auth.UserID,
	}, nil
}

// Run starts the Socket Mode event loop. It blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	go a.eventLoop(ctx)
	return a.socket.RunContext(ctx)
}

func (a *App) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-a.socket.Events:
			switch evt.Type {

			case socketmode.EventTypeConnecting:
				log.Println("connecting to Slack...")

			case socketmode.EventTypeConnectionError:
				log.Println("connection error, will retry")

			case socketmode.EventTypeConnected:
				log.Println("connected to Slack")

			case socketmode.EventTypeEventsAPI:
				a.socket.Ack(*evt.Request)
				apiEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					log.Printf("unexpected EventsAPI payload type: %T", evt.Data)
					continue
				}
				a.handleEventsAPI(ctx, apiEvent)

			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					log.Printf("unexpected slash command payload type: %T", evt.Data)
					continue
				}
				payload := a.handleSlashCommand(cmd)
				a.socket.Ack(*evt.Request, payload)

			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					log.Printf("unexpected interaction payload type: %T", evt.Data)
					continue
				}
				a.socket.Ack(*evt.Request)
				a.handleInteraction(ctx, callback)
			}
		}
	}
}

func (a *App) handleEventsAPI(ctx context.Context, apiEvent slackevents.EventsAPIEvent) {
	if apiEvent.Type != slackevents.CallbackEvent {
		return
	}

	switch ev := apiEvent.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		a.handleMessage(ctx, ev)
	}
}

// handleMessage is the core translation fan-out: for a new message, figure
// out which languages the other channel members want to read in, translate
// once per distinct language, and deliver each translation as an ephemeral
// message visible only to the members who prefer that language.
func (a *App) handleMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	// Ignore anything that isn't a plain new human message: bot messages,
	// message_changed/deleted subtypes, thread broadcasts we've already
	// processed, etc. This keeps the fan-out from firing twice per message.
	if ev.SubType != "" || ev.BotID != "" || ev.User == "" || strings.TrimSpace(ev.Text) == "" {
		return
	}
	if ev.User == a.botUserID {
		return
	}

	members, err := a.channelMembers(ev.Channel)
	if err != nil {
		log.Printf("could not list members of %s: %v", ev.Channel, err)
		return
	}

	authorLang, authorHasLang, err := a.store.GetUserLang(ev.User)
	if err != nil {
		log.Printf("could not load author language: %v", err)
	}

	// Group recipients by target language so we translate once per
	// distinct language, not once per person.
	recipientsByLang := make(map[string][]string) // lang code -> user IDs
	for _, member := range members {
		if member == ev.User || member == a.botUserID {
			continue
		}
		lang, found, err := a.store.GetUserLang(member)
		if err != nil {
			log.Printf("could not load language for %s: %v", member, err)
			continue
		}
		if !found {
			lang = FallbackLangCode
		}
		// Skip translating into the author's own language - if they wrote
		// it in that language already, a translation adds no value (and
		// just spends a free-tier request for nothing).
		if authorHasLang && NormalizeLangCode(lang) == NormalizeLangCode(authorLang) {
			continue
		}
		recipientsByLang[lang] = append(recipientsByLang[lang], member)
	}

	if len(recipientsByLang) == 0 {
		return
	}

	// Remember the original text once, so button clicks later can rebuild
	// any view without re-fetching from Slack.
	if err := a.store.SaveMessage(ev.Channel, ev.TimeStamp, ev.Text); err != nil {
		log.Printf("could not save original message: %v", err)
	}

	for lang, users := range recipientsByLang {
		langName := LangName(lang)
		result, err := a.pool.Translate(ctx, ev.Text, lang, langName)
		if err != nil {
			log.Printf("translation to %s failed: %v", lang, err)
			continue
		}

		val := actionValue{
			Channel:    ev.Channel,
			TS:         ev.TimeStamp,
			TargetLang: lang,
			TargetName: langName,
		}
		blocks := BuildTranslationBlocks(result.Text, viewTranslated, val, result.Model, result.FromCache)

		for _, user := range users {
			_, err := a.api.PostEphemeral(ev.Channel, user, slack.MsgOptionBlocks(blocks...))
			if err != nil {
				log.Printf("could not post ephemeral translation to %s: %v", user, err)
			}
		}
	}
}

// channelMembers lists human, non-deleted members of a channel.
func (a *App) channelMembers(channel string) ([]string, error) {
	var all []string
	cursor := ""
	for {
		members, nextCursor, err := a.api.GetUsersInConversation(&slack.GetUsersInConversationParameters{
			ChannelID: channel,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, members...)
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return all, nil
}

// handleSlashCommand implements "/idioma <code>" so each person can set
// their own preferred reading language (e.g. "/idioma es-mx"). Calling it
// with no argument prints the supported codes.
func (a *App) handleSlashCommand(cmd slack.SlashCommand) map[string]interface{} {
	arg := strings.TrimSpace(cmd.Text)

	if arg == "" || arg == "help" {
		return ephemeralResponse(
			"Supported languages:\n" + SupportedLangsHelp() +
				"\nUse `/idioma <code>` to choose yours, e.g. `/idioma es-mx`.")
	}

	code := NormalizeLangCode(arg)
	if !IsKnownLang(code) {
		return ephemeralResponse(fmt.Sprintf(
			"I don't know the language `%s`. Use `/idioma` with no arguments to see the list of supported codes.", arg))
	}

	if err := a.store.SetUserLang(cmd.UserID, code); err != nil {
		log.Printf("could not save language preference for %s: %v", cmd.UserID, err)
		return ephemeralResponse("There was an error saving your preference, try again in a moment.")
	}

	return ephemeralResponse(fmt.Sprintf("All set! I'll show you the translations in *%s*.", LangName(code)))
}

func ephemeralResponse(text string) map[string]interface{} {
	return map[string]interface{}{
		"response_type": "ephemeral",
		"text":          text,
	}
}

// handleInteraction handles clicks on the "View original" / "View translated"
// / "View in English" buttons. It rebuilds the requested view and replaces
// the ephemeral message in place via response_url.
func (a *App) handleInteraction(ctx context.Context, callback slack.InteractionCallback) {
	if callback.Type != slack.InteractionTypeBlockActions || len(callback.ActionCallback.BlockActions) == 0 {
		return
	}
	action := callback.ActionCallback.BlockActions[0]

	val, err := decodeActionValue(action.Value)
	if err != nil {
		log.Printf("could not decode action value: %v", err)
		return
	}

	original, found, err := a.store.GetMessage(val.Channel, val.TS)
	if err != nil || !found {
		log.Printf("could not load original message for %s/%s: %v", val.Channel, val.TS, err)
		return
	}

	var (
		blocks     []slack.Block
		targetView view
	)

	switch action.ActionID {
	case "show_original":
		targetView = viewOriginal
		blocks = BuildTranslationBlocks(original, viewOriginal, val, "", false)

	case "show_english":
		targetView = viewEnglish
		result, err := a.pool.Translate(ctx, original, "en", "English")
		if err != nil {
			log.Printf("english translation failed: %v", err)
			return
		}
		blocks = BuildTranslationBlocks(result.Text, viewEnglish, val, result.Model, result.FromCache)

	case "show_translated":
		targetView = viewTranslated
		result, err := a.pool.Translate(ctx, original, val.TargetLang, val.TargetName)
		if err != nil {
			log.Printf("translation failed: %v", err)
			return
		}
		blocks = BuildTranslationBlocks(result.Text, viewTranslated, val, result.Model, result.FromCache)

	default:
		return
	}
	_ = targetView // kept for readability/future use (e.g. metrics per view)

	if err := postToResponseURL(callback.ResponseURL, blocks); err != nil {
		log.Printf("could not update ephemeral message: %v", err)
	}
}

// postToResponseURL replaces the ephemeral message in place. Slack's
// response_url for block actions defaults to replace_original=true, which
// is exactly the "toggle the view" behavior we want.
func postToResponseURL(responseURL string, blocks []slack.Block) error {
	msg := slack.WebhookMessage{
		ReplaceOriginal: true,
		Blocks:          &slack.Blocks{BlockSet: blocks},
	}
	return slack.PostWebhookCustomHTTPContext(context.Background(), responseURL, http.DefaultClient, &msg)
}

// Ensure the store package's concrete type satisfies LangStore at compile
// time - this line only exists to catch interface drift early.
var _ LangStore = (*store.Store)(nil)
