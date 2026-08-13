package ui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/go-chi/chi/v5"
	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

func (ur *Router) channelStates(req *http.Request) (pushover, campfire bool) {
	ctx := req.Context()
	pushover = ur.Settings.IsSet(ctx, services.SettingPushoverAppToken) &&
		ur.Settings.IsSet(ctx, services.SettingPushoverUserKey)
	campfire = ur.Settings.IsSet(ctx, services.SettingCampfireBaseURL) &&
		ur.Settings.IsSet(ctx, services.SettingCampfireBotKey)
	return pushover, campfire
}

func (ur *Router) alertsPage(w http.ResponseWriter, req *http.Request) {
	ur.renderAlerts(w, req, "")
}

func (ur *Router) renderAlerts(w http.ResponseWriter, req *http.Request, errMsg string) {
	alerts, err := ur.Alerts.List(req.Context())
	if err != nil {
		log.Error().Err(err).Msg("list alerts")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	po, cf := ur.channelStates(req)
	v := templates.AlertsView{Alerts: alerts, ErrMsg: errMsg, PushoverConfigured: po, CampfireConfigured: cf}
	// region-only for HTMX flows (delete, edit-cancel); full page otherwise
	if req.Header.Get("HX-Request") == "true" && req.Method == http.MethodGet && errMsg == "" && req.Header.Get("HX-History-Restore-Request") != "true" {
		ur.render(w, req, http.StatusOK, templates.AlertsRegion(v))
		return
	}
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusBadRequest
	}
	ur.render(w, req, status, templates.AlertsPage(v, nosurf.Token(req)))
}

func targetTypeOf(raw string) string {
	t, _ := services.ParseTarget(raw)
	return t.Type
}

// alertFromForm builds an Alert from the shared create/edit form.
func alertFromForm(req *http.Request) (services.Alert, error) {
	var a services.Alert
	if err := req.ParseForm(); err != nil {
		return a, err
	}
	a.Name = req.PostFormValue("name")
	a.Query = strings.TrimSpace(req.PostFormValue("query"))
	intervalS, _ := strconv.ParseInt(req.PostFormValue("interval_s"), 10, 64)
	cooldownM, _ := strconv.ParseInt(req.PostFormValue("cooldown_m"), 10, 64)
	a.CheckIntervalMs = intervalS * 1000
	a.CooldownMs = cooldownM * 60_000
	a.Enabled = req.PostFormValue("enabled") != ""

	t := services.Target{Type: req.PostFormValue("target_type")}
	switch t.Type {
	case "pushover":
		t.Device = strings.TrimSpace(req.PostFormValue("po_device"))
		t.Priority, _ = strconv.Atoi(req.PostFormValue("po_priority"))
		t.Sound = strings.TrimSpace(req.PostFormValue("po_sound"))
	case "campfire":
		t.RoomID, _ = strconv.ParseInt(req.PostFormValue("cf_room_id"), 10, 64)
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return a, err
	}
	a.Target = string(raw)
	return a, nil
}

// channelConfigured mirrors the selector rule: only configured channels are
// offered, and the server enforces it (except updates keeping their type).
func (ur *Router) channelConfigured(req *http.Request, targetRaw string) bool {
	t, err := services.ParseTarget(targetRaw)
	if err != nil {
		return false
	}
	po, cf := ur.channelStates(req)
	return (t.Type == "pushover" && po) || (t.Type == "campfire" && cf)
}

func (ur *Router) createAlert(w http.ResponseWriter, req *http.Request) {
	a, err := alertFromForm(req)
	if err == nil && !ur.channelConfigured(req, a.Target) {
		ur.renderAlerts(w, req, "Configure the chosen notification channel in Settings before creating alerts.")
		return
	}
	if err == nil {
		err = ur.Alerts.Create(req.Context(), a)
	}
	if err != nil {
		ur.renderAlerts(w, req, err.Error())
		return
	}
	http.Redirect(w, req, "/alerts", http.StatusSeeOther)
}

func (ur *Router) editAlertPage(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	a, err := ur.Alerts.Get(req.Context(), id)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	po, cf := ur.channelStates(req)
	ur.render(w, req, http.StatusOK, templates.AlertEditRow(a, po, cf, nosurf.Token(req)))
}

func (ur *Router) updateAlert(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	a, ferr := alertFromForm(req)
	a.ID = id
	if ferr == nil && !ur.channelConfigured(req, a.Target) {
		// keeping the existing type is always allowed, switching to an
		// unconfigured one is not
		if existing, gerr := ur.Alerts.Get(req.Context(), id); gerr != nil ||
			targetTypeOf(existing.Target) != targetTypeOf(a.Target) {
			ur.renderAlerts(w, req, "Configure the chosen notification channel in Settings first.")
			return
		}
	}
	if ferr == nil {
		ferr = ur.Alerts.Update(req.Context(), a)
	}
	if ferr != nil {
		// full page with the error banner; simplest correct feedback path
		ur.renderAlerts(w, req, ferr.Error())
		return
	}
	ur.renderAlerts(w, req, "")
}

func (ur *Router) deleteAlert(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := ur.Alerts.Delete(req.Context(), id); err != nil {
		log.Error().Err(err).Int64("id", id).Msg("delete alert")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	ur.renderAlerts(w, req, "")
}

// POST /alerts/{id}/test - pure dry run; never notifies.
func (ur *Router) dryRunAlert(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	a, err := ur.Alerts.Get(req.Context(), id)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	v, err := query.Validate(a.Query)
	if err != nil {
		ur.render(w, req, http.StatusOK, templates.DryRunResult(a.Name, nil, nil, humanizeSQLError(err)))
		return
	}
	res, err := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL())
	if err != nil {
		ur.render(w, req, http.StatusOK, templates.DryRunResult(a.Name, nil, nil, err.Error()))
		return
	}

	const maxDryRunRows = 10
	cells := make([][]string, 0, min(len(res.Rows), maxDryRunRows))
	for i, row := range res.Rows {
		if i >= maxDryRunRows {
			break
		}
		cs := make([]string, len(row))
		for j, cell := range row {
			cs[j] = cellString(cell)
		}
		cells = append(cells, cs)
	}
	ur.render(w, req, http.StatusOK, templates.DryRunResult(a.Name, res.Columns, cells, ""))
}
