package web

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// commonZones is what the field suggests. It is a shortlist and not a closed
// set: anything the tz database names is accepted, because no list anybody
// maintains by hand covers where people actually are.
func commonZones() []string {
	return []string{
		"UTC",
		"America/Sao_Paulo", "America/Argentina/Buenos_Aires", "America/Bogota",
		"America/Mexico_City", "America/New_York", "America/Chicago",
		"America/Denver", "America/Los_Angeles", "America/Vancouver",
		"Europe/Lisbon", "Europe/London", "Europe/Dublin", "Europe/Madrid",
		"Europe/Paris", "Europe/Berlin", "Europe/Amsterdam", "Europe/Rome",
		"Europe/Warsaw", "Europe/Kyiv", "Europe/Istanbul", "Europe/Moscow",
		"Africa/Lagos", "Africa/Nairobi", "Africa/Johannesburg", "Africa/Cairo",
		"Asia/Jerusalem", "Asia/Dubai", "Asia/Karachi", "Asia/Kolkata",
		"Asia/Bangkok", "Asia/Jakarta", "Asia/Shanghai", "Asia/Hong_Kong",
		"Asia/Singapore", "Asia/Seoul", "Asia/Tokyo",
		"Australia/Perth", "Australia/Sydney", "Pacific/Auckland",
	}
}

func (h *Handler) profile(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, "Profile", profilePage(currentUser(r), ""))
}

func (h *Handler) saveProfile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	user := currentUser(r)
	wanted := strings.TrimSpace(r.FormValue("time_zone"))

	if _, err := time.LoadLocation(wanted); err != nil {
		user.TimeZone = wanted
		w.WriteHeader(http.StatusBadRequest)
		h.page(w, r, "Profile", profilePage(user, "no zone is named "+wanted))
		return
	}

	if err := h.store.SetTimeZone(r.Context(), user.ID, wanted); err != nil {
		h.logger.ErrorContext(r.Context(), "could not save a time zone",
			"email", user.Email, "zone", wanted, "error", err)
		http.Error(w, "could not save that", http.StatusInternalServerError)
		return
	}

	h.logger.InfoContext(r.Context(), "an operator changed their time zone",
		"email", user.Email, "zone", wanted)
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

// readingZone is the location a person's recorded instants are rendered in.
// An empty setting is UTC, and so is a name this build cannot resolve: the
// panel showing the moment in the wrong zone would be worse than showing it in
// the one everything is stored in.
func readingZone(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	place, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return place
}

// zone is where the person looking at this request reads a clock.
func zone(ctx context.Context) *time.Location {
	place, held := ctx.Value(zoneKey{}).(*time.Location)
	if !held || place == nil {
		return time.UTC
	}
	return place
}

// zoneName is what the panel says it is showing, so a column of timestamps is
// never ambiguous about which zone it is in.
func zoneName(ctx context.Context) string {
	return zone(ctx).String()
}
