package ui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/go-chi/chi/v5"
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
	targets, err := ur.ScrapeTargets.List(ctx)
	if err != nil {
		log.Error().Err(err).Msg("list scrape targets")
		return templates.SettingsView{}, err
	}
	return templates.SettingsView{
		Keys:   keys,
		NewKey: newKey,
		ErrMsg: errMsg,
		PushoverConfigured: ur.Settings.IsSet(ctx, services.SettingPushoverAppToken) &&
			ur.Settings.IsSet(ctx, services.SettingPushoverUserKey),
		CampfireBaseURL: base,
		CampfireKeySet:  ur.Settings.IsSet(ctx, services.SettingCampfireBotKey),
		ScrapeTargets:   targets,
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

// Scrape targets

func (ur *Router) createScrapeTarget(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Redirect(w, req, "/settings", http.StatusSeeOther)
		return
	}
	intervalS, _ := strconv.ParseInt(req.PostFormValue("interval_s"), 10, 64)
	t := services.ScrapeTarget{
		Service:          req.PostFormValue("service"),
		Host:             req.PostFormValue("host"),
		URL:              strings.TrimSpace(req.PostFormValue("url")),
		AuthHeader:       req.PostFormValue("auth_header"),
		ScrapeIntervalMs: intervalS * 1000,
		Enabled:          true,
	}
	if strings.TrimSpace(t.Host) == "" {
		if u, err := url.Parse(t.URL); err == nil {
			t.Host = u.Hostname()
		}
	}
	if err := ur.ScrapeTargets.Create(req.Context(), t); err != nil {
		v, verr := ur.settingsView(req, "", "")
		if verr != nil {
			http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
			return
		}
		v.ScrapeErrMsg = err.Error()
		ur.render(w, req, http.StatusBadRequest, templates.SettingsPage(v, nosurf.Token(req)))
		return
	}
	http.Redirect(w, req, "/settings", http.StatusSeeOther)
}

func (ur *Router) renderScrapeTargetsSection(w http.ResponseWriter, req *http.Request) {
	targets, err := ur.ScrapeTargets.List(req.Context())
	if err != nil {
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	ur.render(w, req, http.StatusOK, templates.ScrapeTargetsSection(targets, "", nosurf.Token(req)))
}

func (ur *Router) toggleScrapeTarget(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	targets, err := ur.ScrapeTargets.List(req.Context())
	if err != nil {
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	for _, t := range targets {
		if t.ID == id {
			if err := ur.ScrapeTargets.SetEnabled(req.Context(), id, !t.Enabled); err != nil {
				log.Error().Err(err).Int64("id", id).Msg("toggle scrape target")
			}
			break
		}
	}
	ur.renderScrapeTargetsSection(w, req)
}

func (ur *Router) deleteScrapeTarget(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := ur.ScrapeTargets.Delete(req.Context(), id); err != nil {
		log.Error().Err(err).Int64("id", id).Msg("delete scrape target")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	ur.renderScrapeTargetsSection(w, req)
}
