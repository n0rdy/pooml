package ui

import (
	"context"
	"fmt"
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
	logsDays, metricsDays, firingsDays := ur.retentionDays(ctx)
	return templates.SettingsView{
		Keys:   keys,
		NewKey: newKey,
		ErrMsg: errMsg,
		PushoverConfigured: ur.Settings.IsSet(ctx, services.SettingPushoverAppToken) &&
			ur.Settings.IsSet(ctx, services.SettingPushoverUserKey),
		CampfireBaseURL:      base,
		CampfireKeySet:       ur.Settings.IsSet(ctx, services.SettingCampfireBotKey),
		ScrapeTargets:        targets,
		RetentionLogsDays:    logsDays,
		RetentionMetricsDays: metricsDays,
		RetentionFiringsDays: firingsDays,
		Backup:               ur.backupView(ctx),
	}, nil
}

func (ur *Router) retentionDays(ctx context.Context) (int, int, int) {
	return services.RetentionDays(ctx, ur.Settings, services.SettingRetentionLogsDays, services.RetentionDefaultLogsDays),
		services.RetentionDays(ctx, ur.Settings, services.SettingRetentionMetricsDays, services.RetentionDefaultMetricsDays),
		services.RetentionDays(ctx, ur.Settings, services.SettingRetentionFiringsDays, services.RetentionDefaultFiringsDays)
}

func (ur *Router) backupView(ctx context.Context) templates.BackupView {
	get := func(key string) string { v, _ := ur.Settings.Get(ctx, key); return v }
	enabled, _ := strconv.ParseBool(get(services.SettingBackupEnabled))
	lastRun, _ := strconv.ParseInt(get(services.SettingBackupLastRunAt), 10, 64)
	schedule := get(services.SettingBackupSchedule)
	if schedule == "" {
		schedule = services.BackupDefaultSchedule
	}
	return templates.BackupView{
		Enabled:    enabled,
		Schedule:   schedule,
		Endpoint:   get(services.SettingBackupEndpoint),
		Region:     get(services.SettingBackupRegion),
		Bucket:     get(services.SettingBackupBucket),
		Prefix:     get(services.SettingBackupPrefix),
		CredsSet:   ur.Settings.IsSet(ctx, services.SettingBackupAccessKey) && ur.Settings.IsSet(ctx, services.SettingBackupSecretKey),
		LastRunAt:  lastRun,
		LastResult: get(services.SettingBackupLastResult),
	}
}

// PUT /settings/backup - saves S3 + schedule config, re-renders the section.
func (ur *Router) updateBackupSettings(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	render := func(v templates.BackupView, msg string, ok bool) {
		ur.render(w, req, http.StatusOK, templates.BackupSection(v, msg, ok, nosurf.Token(req)))
	}
	if err := req.ParseForm(); err != nil {
		render(ur.backupView(ctx), "That didn't come through right. Try again.", false)
		return
	}
	schedule := strings.TrimSpace(req.PostFormValue("schedule"))
	if schedule == "" {
		schedule = services.BackupDefaultSchedule
	}
	if err := services.ValidateCron(schedule); err != nil {
		render(ur.backupView(ctx), fmt.Sprintf("That's not a cron expression: %v", err), false)
		return
	}

	plain := map[string]string{
		services.SettingBackupEnabled:  strconv.FormatBool(req.PostFormValue("enabled") != ""),
		services.SettingBackupSchedule: schedule,
		services.SettingBackupEndpoint: strings.TrimSpace(req.PostFormValue("endpoint")),
		services.SettingBackupRegion:   strings.TrimSpace(req.PostFormValue("region")),
		services.SettingBackupBucket:   strings.TrimSpace(req.PostFormValue("bucket")),
		services.SettingBackupPrefix:   strings.Trim(strings.TrimSpace(req.PostFormValue("prefix")), "/"),
	}
	for key, val := range plain {
		if err := ur.Settings.Set(ctx, key, val, false); err != nil {
			log.Error().Err(err).Str("key", key).Msg("save backup setting")
			render(ur.backupView(ctx), "Saving failed - check the logs.", false)
			return
		}
	}
	// blank secret fields keep the stored values (same rule as Pushover)
	for key, form := range map[string]string{
		services.SettingBackupAccessKey: "access_key",
		services.SettingBackupSecretKey: "secret_key",
	} {
		if v := strings.TrimSpace(req.PostFormValue(form)); v != "" {
			if err := ur.Settings.Set(ctx, key, v, true); err != nil {
				log.Error().Err(err).Str("key", key).Msg("save backup secret")
				render(ur.backupView(ctx), "Saving failed - check the logs.", false)
				return
			}
		}
	}
	render(ur.backupView(ctx), "Saved.", true)
}

// POST /settings/backup/run - one manual backup, outcome inline.
func (ur *Router) runBackupNow(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	msg, ok := "Backup uploaded.", true
	if err := ur.Backup.RunNow(ctx); err != nil {
		msg, ok = "Backup failed: "+err.Error(), false
	}
	ur.render(w, req, http.StatusOK, templates.BackupSection(ur.backupView(ctx), msg, ok, nosurf.Token(req)))
}

// PUT /settings/retention - saves all three day counts, re-renders the section.
func (ur *Router) updateRetentionSettings(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	render := func(logs, metrics, firings int, msg string, ok bool) {
		ur.render(w, req, http.StatusOK,
			templates.RetentionSection(logs, metrics, firings, msg, ok, nosurf.Token(req)))
	}
	curLogs, curMetrics, curFirings := ur.retentionDays(ctx)

	if err := req.ParseForm(); err != nil {
		render(curLogs, curMetrics, curFirings, "That didn't come through right. Try again.", false)
		return
	}
	fields := []struct {
		form string
		key  string
	}{
		{"logs_days", services.SettingRetentionLogsDays},
		{"metrics_days", services.SettingRetentionMetricsDays},
		{"alert_firings_days", services.SettingRetentionFiringsDays},
	}
	values := make([]int, len(fields))
	for i, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(req.PostFormValue(f.form)))
		if err != nil || n < services.RetentionMinDays || n > services.RetentionMaxDays {
			render(curLogs, curMetrics, curFirings,
				fmt.Sprintf("Each value must be a whole number of days between %d and %d.",
					services.RetentionMinDays, services.RetentionMaxDays), false)
			return
		}
		values[i] = n
	}
	for i, f := range fields {
		if err := ur.Settings.Set(ctx, f.key, strconv.Itoa(values[i]), false); err != nil {
			log.Error().Err(err).Str("key", f.key).Msg("save retention setting")
			render(curLogs, curMetrics, curFirings, "Saving failed - check the logs.", false)
			return
		}
	}
	render(values[0], values[1], values[2], "Saved. The next hourly sweep uses these values.", true)
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
