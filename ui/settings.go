package ui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

func (ur *Router) settingsView(req *http.Request, newKey, errMsg string) (templates.SettingsView, error) {
	ctx := req.Context()
	keys, err := ur.ApiKeys.List(ctx)
	if err != nil {
		log.Error().Err(err).Msg("list api keys")
		return templates.SettingsView{}, err
	}
	base, _ := ur.Settings.Get(ctx, services.SettingCampfireBaseURL)
	return templates.SettingsView{
		Keys:   keys,
		NewKey: newKey,
		ErrMsg: errMsg,
		PushoverConfigured: ur.Settings.IsSet(ctx, services.SettingPushoverAppToken) &&
			ur.Settings.IsSet(ctx, services.SettingPushoverUserKey),
		CampfireBaseURL: base,
		CampfireKeySet:  ur.Settings.IsSet(ctx, services.SettingCampfireBotKey),
	}, nil
}

// savePushover stores credentials; blank fields keep existing values so a
// saved form never wipes a configured secret.
func (ur *Router) savePushover(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err == nil {
		ctx := req.Context()
		if v := strings.TrimSpace(req.PostFormValue("app_token")); v != "" {
			if err := ur.Settings.Set(ctx, services.SettingPushoverAppToken, v, true); err != nil {
				log.Error().Err(err).Msg("save pushover token")
			}
		}
		if v := strings.TrimSpace(req.PostFormValue("user_key")); v != "" {
			if err := ur.Settings.Set(ctx, services.SettingPushoverUserKey, v, true); err != nil {
				log.Error().Err(err).Msg("save pushover user")
			}
		}
	}
	http.Redirect(w, req, "/settings", http.StatusSeeOther)
}

func (ur *Router) saveCampfire(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err == nil {
		ctx := req.Context()
		if v := strings.TrimSpace(req.PostFormValue("base_url")); v != "" {
			if err := ur.Settings.Set(ctx, services.SettingCampfireBaseURL, v, false); err != nil {
				log.Error().Err(err).Msg("save campfire url")
			}
		}
		if v := strings.TrimSpace(req.PostFormValue("bot_key")); v != "" {
			if err := ur.Settings.Set(ctx, services.SettingCampfireBotKey, v, true); err != nil {
				log.Error().Err(err).Msg("save campfire key")
			}
		}
	}
	http.Redirect(w, req, "/settings", http.StatusSeeOther)
}

// Channel test buttons return a small inline fragment.
func (ur *Router) testPushover(w http.ResponseWriter, req *http.Request) {
	ur.renderChannelTest(w, req, ur.Notifier.TestPushover(req.Context()))
}

func (ur *Router) testCampfire(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	roomID, err := strconv.ParseInt(req.PostFormValue("room_id"), 10, 64)
	if err != nil || roomID <= 0 {
		ur.render(w, req, http.StatusOK, templates.ChannelTestResult("enter a room id first"))
		return
	}
	ur.renderChannelTest(w, req, ur.Notifier.TestCampfire(req.Context(), roomID))
}

func (ur *Router) renderChannelTest(w http.ResponseWriter, req *http.Request, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	ur.render(w, req, http.StatusOK, templates.ChannelTestResult(msg))
}

// keep nosurf referenced for the settings handlers in router.go after edits
var _ = nosurf.Token
