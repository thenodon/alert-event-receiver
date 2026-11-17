package slaproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func datasetPath(value string) *string { return &value }
func icsEvent(start, end string) string {
	event := "BEGIN:VEVENT\r\nUID:test@example\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;VALUE=DATE:" + start + "\r\n"
	if end != "" {
		event += "DTEND;VALUE=DATE:" + end + "\r\n"
	}
	return event + "END:VEVENT\r\n"
}
func icsCalendar(events ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"
}
func writeDataset(t testing.TB, contents string, first, last int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "holidays.ics")
	saveDataset(t, path, contents, first, last)
	return path
}
func saveDataset(t testing.TB, path, contents string, first, last int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(contents))
	manifest := datasetManifest{SchemaVersion: 1, Format: "ics-holiday-dataset", Country: "SE", FirstYear: first, LastYear: last, Source: "test", SourceVersion: "1", ICSSHA256: hex.EncodeToString(sum[:])}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(path+".json", data, 0600); err != nil {
		t.Fatal(err)
	}
}
func fixtureDataset(t testing.TB, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "holidays", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func datasetConfig(t testing.TB, path string) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.SLADefinitions = []Definition{{SLALabel: "sla_id", SLAs: map[string]Policy{"business": fullDayPolicy()}}}
	policy := cfg.SLADefinitions[0].SLAs["business"]
	policy.Holidays = &Holidays{Dataset: datasetPath(path)}
	cfg.SLADefinitions[0].SLAs["business"] = policy
	return cfg
}
func datasetEngine(t *testing.T, cfg Config) *CalendarEngine {
	t.Helper()
	engine, err := newCalendarEngineAt(cfg, CountryHolidays{}, instant(t, "2026-11-17T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestDatasetConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		holidays Holidays
		valid    bool
	}{
		{"dataset", Holidays{Dataset: datasetPath("/etc/cal.ics")}, true},
		{"union", Holidays{Dataset: datasetPath("/etc/cal.ics"), AdditionalDates: []string{"2027-12-24"}}, true},
		{"additional", Holidays{AdditionalDates: []string{"2027-12-24"}}, true},
		{"country", Holidays{Dataset: datasetPath("/etc/cal.ics"), Country: "SE"}, false},
		{"subdivision", Holidays{Dataset: datasetPath("/etc/cal.ics"), Subdivision: "BY"}, false},
		{"empty", Holidays{Dataset: datasetPath("")}, false},
		{"relative", Holidays{Dataset: datasetPath("cal.ics")}, false},
		{"URL", Holidays{Dataset: datasetPath("https://example.test/cal.ics")}, false},
		{"extension", Holidays{Dataset: datasetPath("/etc/cal.txt")}, false},
		{"invalid date", Holidays{Dataset: datasetPath("/etc/cal.ics"), AdditionalDates: []string{"2026-02-30"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fullDayPolicy()
			p.Holidays = &tc.holidays
			if err := validatePolicy(p); (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	text := strings.Replace(exampleConfig, "country: SE", `dataset: ""`, 1)
	if _, err := LoadConfig(strings.NewReader(text)); err == nil {
		t.Fatal("explicit empty path accepted")
	}
	text = strings.Replace(exampleConfig, "country: SE", `dataset: "/etc/cal.ics"`+"\n          typo: true", 1)
	if _, err := LoadConfig(strings.NewReader(text)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDatasetICSProfile(t *testing.T) {
	contents := icsCalendar(icsEvent("20261224", "20261227"), icsEvent("20261225", ""),
		strings.Replace(icsEvent("20260101", ""), "UID:", "SUMMARY:long\\, escaped\r\n continuation\r\nUID:", 1))
	dates, err := parseHolidayICS([]byte(contents), 2020, 2040)
	if err != nil || len(dates) != 4 || !dates["2026-12-24"] || !dates["2026-12-26"] || dates["2026-12-27"] {
		t.Fatal(dates, err)
	}
	event := icsEvent("20260101", "20260102")
	tests := map[string]string{
		"no envelope":       event,
		"truncated":         strings.TrimSuffix(icsCalendar(event), "END:VCALENDAR\r\n"),
		"trailing":          icsCalendar(event) + "unexpected\r\n",
		"two calendars":     icsCalendar(event) + icsCalendar(event),
		"empty":             icsCalendar(),
		"version":           strings.Replace(icsCalendar(event), "VERSION:2.0", "VERSION:1.0", 1),
		"duplicate version": strings.Replace(icsCalendar(event), "VERSION:2.0", "VERSION:2.0\r\nVERSION:2.0", 1),
		"missing start":     icsCalendar("BEGIN:VEVENT\r\nEND:VEVENT\r\n"),
		"duplicate start":   icsCalendar(strings.Replace(event, "DTSTART;VALUE=DATE:20260101", "DTSTART;VALUE=DATE:20260101\r\nDTSTART;VALUE=DATE:20260101", 1)),
		"duplicate end":     icsCalendar(strings.Replace(event, "DTEND;VALUE=DATE:20260102", "DTEND;VALUE=DATE:20260102\r\nDTEND;VALUE=DATE:20260102", 1)),
		"date-time":         icsCalendar(strings.Replace(event, "DTSTART;VALUE=DATE:20260101", "DTSTART:20260101T000000Z", 1)),
		"TZID":              icsCalendar(strings.Replace(event, "DTSTART;VALUE=DATE:", "DTSTART;VALUE=DATE;TZID=Europe/Stockholm:", 1)),
		"bad day":           icsCalendar(icsEvent("20260230", "")),
		"backward":          icsCalendar(icsEvent("20260102", "20260101")),
		"empty range":       icsCalendar(icsEvent("20260101", "20260101")),
		"large range":       icsCalendar(icsEvent("20200101", "20400101")),
		"outside coverage":  icsCalendar(icsEvent("20190101", "")),
		"bad syntax":        icsCalendar(strings.Replace(event, "UID:test@example", "no colon", 1)),
		"nested":            icsCalendar(strings.Replace(event, "END:VEVENT", "BEGIN:VALARM\r\nEND:VALARM\r\nEND:VEVENT", 1)),
		"nested params":     icsCalendar(strings.Replace(event, "END:VEVENT", "BEGIN;X-TEST=1:VALARM\r\nEND:VALARM\r\nEND:VEVENT", 1)),
		"other component":   strings.ReplaceAll(icsCalendar(event), "VEVENT", "VTODO"),
		"nul":               icsCalendar(event) + "\x00",
		"invalid UTF8":      icsCalendar(event) + "\xff",
		"line limit":        icsCalendar(strings.Replace(event, "UID:", "SUMMARY:"+strings.Repeat("x", maxDatasetLineBytes)+"\r\nUID:", 1)),
	}
	for _, property := range []string{"RRULE:FREQ=YEARLY", "RDATE;VALUE=DATE:20270101", "EXDATE;VALUE=DATE:20260101", "RECURRENCE-ID;VALUE=DATE:20260101", "DURATION:P1D", "EXRULE:FREQ=YEARLY", "STATUS:CANCELLED"} {
		tests[property] = icsCalendar(strings.Replace(event, "END:VEVENT", property+"\r\nEND:VEVENT", 1))
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseHolidayICS([]byte(contents), 2020, 2040); err == nil {
				t.Fatal("invalid ICS accepted")
			}
		})
	}
	t.Run("event limit", func(t *testing.T) {
		if _, err := parseHolidayICS([]byte(icsCalendar(strings.Repeat("BEGIN:VEVENT\nEND:VEVENT\n", maxDatasetEvents+1))), 2020, 2040); err == nil {
			t.Fatal("event limit ignored")
		}
	})
	t.Run("expansion limit", func(t *testing.T) {
		if _, err := parseHolidayICS([]byte(icsCalendar(strings.Repeat(icsEvent("20200101", "20300108"), 55))), 2020, 2040); err == nil {
			t.Fatal("expansion limit ignored")
		}
	})
	t.Run("byte limit", func(t *testing.T) {
		if _, err := parseHolidayICS(bytes.Repeat([]byte("x"), maxDatasetBytes+1), 2020, 2040); err == nil {
			t.Fatal("byte limit ignored")
		}
	})
}

func TestDatasetFilesAndManifest(t *testing.T) {
	path := writeDataset(t, icsCalendar(icsEvent("20260101", "")), 2020, 2040)
	raw, _ := os.ReadFile(path + ".json")
	var original map[string]any
	_ = json.Unmarshal(raw, &original)
	for _, field := range []string{"schema_version", "format", "first_year", "last_year", "source", "source_version", "ics_sha256"} {
		t.Run("missing "+field, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range original {
				m[k] = v
			}
			delete(m, field)
			data, _ := json.Marshal(m)
			if _, err := parseDatasetManifest(data); err == nil {
				t.Fatal("missing field accepted")
			}
		})
	}
	for name, data := range map[string][]byte{
		"unknown":   append(raw[:len(raw)-1:len(raw)-1], []byte(`,"unknown":1}`)...),
		"duplicate": []byte(`{"schema_version":1,"schema_version":1}`),
		"array":     []byte(`[]`), "null": []byte(`null`), "nested": []byte(`{"source":{}}`), "trailing": append(append([]byte{}, raw...), raw...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDatasetManifest(data); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	for name, change := range map[string]func(map[string]any){
		"schema": func(m map[string]any) { m["schema_version"] = 2 }, "format": func(m map[string]any) { m["format"] = "other" },
		"reversed": func(m map[string]any) { m["first_year"] = 2041 }, "zero year": func(m map[string]any) { m["first_year"] = 0 },
		"last year": func(m map[string]any) { m["last_year"] = 10000 }, "upper SHA": func(m map[string]any) { m["ics_sha256"] = strings.ToUpper(m["ics_sha256"].(string)) },
		"short SHA": func(m map[string]any) { m["ics_sha256"] = "123" }, "wrong SHA": func(m map[string]any) { m["ics_sha256"] = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range original {
				m[k] = v
			}
			change(m)
			data, _ := json.Marshal(m)
			_ = os.WriteFile(path+".json", data, 0600)
			if _, err := loadHolidayDataset(path); err == nil {
				t.Fatal("invalid manifest accepted")
			}
			_ = os.WriteFile(path+".json", raw, 0600)
		})
	}
	for _, tc := range []struct {
		name     string
		file     string
		contents []byte
	}{
		{"empty ICS", path, nil}, {"oversize ICS", path, bytes.Repeat([]byte("x"), maxDatasetBytes+1)},
		{"empty manifest", path + ".json", nil}, {"oversize manifest", path + ".json", bytes.Repeat([]byte("x"), maxManifestBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, _ := os.ReadFile(tc.file)
			defer os.WriteFile(tc.file, saved, 0600)
			_ = os.WriteFile(tc.file, tc.contents, 0600)
			if _, err := loadHolidayDataset(path); err == nil {
				t.Fatal("invalid files accepted")
			}
		})
	}
	if _, err := readDatasetFile(t.TempDir(), maxDatasetBytes); err == nil {
		t.Fatal("directory accepted")
	}
	_ = os.Remove(path + ".json")
	if _, err := loadHolidayDataset(path); err == nil {
		t.Fatal("missing manifest accepted")
	}
	_ = os.Remove(path)
	if _, err := loadHolidayDataset(path); err == nil {
		t.Fatal("missing ICS accepted")
	}
}

func TestGeneratedHolidayDatasetsAndCivilDates(t *testing.T) {
	for _, tc := range []struct {
		file, date string
		holiday    bool
	}{
		{"holidays_se.ics", "2026-04-03", true}, {"holidays_se.ics", "2027-03-26", true},
		{"holidays_se.ics", "2026-11-11", false}, {"holidays_se.ics", "2026-12-24", false},
		{"holidays_de_by.ics", "2026-01-06", true}, {"holidays_de_by.ics", "2026-06-04", true},
		{"holidays_de_by.ics", "2026-10-03", true}, {"holidays_de_by.ics", "2026-11-11", false},
	} {
		t.Run(tc.file+tc.date, func(t *testing.T) {
			d, err := loadHolidayDataset(fixtureDataset(t, tc.file))
			if err != nil || d.dates[tc.date] != tc.holiday {
				t.Fatal(d, err)
			}
		})
	}
	path := writeDataset(t, icsCalendar(icsEvent("20200101", ""), icsEvent("20260329", ""), icsEvent("20261025", "")), 2020, 2040)
	cfg := datasetConfig(t, path)
	p := cfg.SLADefinitions[0].SLAs["business"]
	p.Timezone = "Pacific/Kiritimati"
	cfg.SLADefinitions[0].SLAs["business"] = p
	engine := datasetEngine(t, cfg)
	decision, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, "2020-01-01T00:00:00+14:00"))
	if err != nil || decision.Eligible || decision.Reason != "holiday" {
		t.Fatal(decision, err)
	}
	p.Timezone = "Europe/Stockholm"
	p.Holidays.AdditionalDates = []string{"2026-03-29", "2041-01-01"}
	cfg.SLADefinitions[0].SLAs["business"] = p
	engine = datasetEngine(t, cfg)
	for _, at := range []string{"2026-03-29T01:30:00+01:00", "2026-10-25T02:30:00+02:00", "2026-10-25T02:30:00+01:00"} {
		d, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, at))
		if err != nil || d.Eligible {
			t.Fatal(d, err)
		}
		if strings.HasPrefix(at, "2026-03") && d.Reason != "additional_date" {
			t.Fatal(d)
		}
	}
	if _, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, "2041-01-01T00:00:00Z")); !errors.Is(err, errDatasetCoverage) {
		t.Fatal("additional date hid missing coverage", err)
	}
	// An overnight carry is excluded according to the date being evaluated.
	p.Schedule = map[string][]string{"saturday": {"22:00-06:00"}}
	cfg.SLADefinitions[0].SLAs["business"] = p
	engine = datasetEngine(t, cfg)
	d, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, "2026-03-29T00:30:00+01:00"))
	if err != nil || d.Eligible || d.Reason != "additional_date" {
		t.Fatal(d, err)
	}
}

func TestDatasetCoverageAndReadiness(t *testing.T) {
	path := writeDataset(t, icsCalendar(icsEvent("20260101", "")), 2026, 2026)
	cfg := datasetConfig(t, path)
	if _, err := newCalendarEngineAt(cfg, CountryHolidays{}, instant(t, "2026-11-17T00:00:00Z")); err == nil {
		t.Fatal("insufficient startup horizon accepted")
	}
	cfg.EvaluationHorizonDays = 10
	engine, err := newCalendarEngineAt(cfg, CountryHolidays{}, instant(t, "2026-11-17T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"2025-12-31T12:00:00Z", "2027-01-01T12:00:00Z"} {
		d, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, at))
		var evaluation *EvaluationError
		if !errors.As(err, &evaluation) || evaluation.Reason != "calendar_error" || d.PolicyVersion != "" {
			t.Fatal(d, err)
		}
	}
	p := cfg.SLADefinitions[0].SLAs["business"]
	p.Schedule = map[string][]string{"monday": {}}
	cfg.SLADefinitions[0].SLAs["business"] = p
	engine, err = newCalendarEngineAt(cfg, CountryHolidays{}, instant(t, "2026-11-17T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, "2026-12-28T00:00:00Z")); !errors.Is(err, errDatasetCoverage) {
		t.Fatal("unscheduled dates bypassed coverage", err)
	}
	// Real handler clock crosses the rolling coverage warning boundary, with no reload.
	path = fixtureDataset(t, "holidays_se.ics")
	cfg = datasetConfig(t, path)
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tc := range []struct {
		at     string
		status int
	}{{"2026-11-17T12:00:00Z", 200}, {"2040-12-31T12:00:00Z", 503}} {
		s.now = func() time.Time { return instant(t, tc.at) }
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != tc.status {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestDatasetSemanticVersionsCacheAndIsolation(t *testing.T) {
	contents := icsCalendar(icsEvent("20260101", ""), icsEvent("20261225", ""))
	path := writeDataset(t, contents, 2020, 2040)
	other := writeDataset(t, contents, 2020, 2040)
	cfg := datasetConfig(t, path)
	p := cfg.SLADefinitions[0].SLAs["business"]
	p.Holidays = &Holidays{Dataset: datasetPath(other)}
	cfg.SLADefinitions[0].SLAs["other"] = p
	first := datasetEngine(t, cfg)
	ref := PolicyRef{"sla_id", "business"}
	otherRef := PolicyRef{"sla_id", "other"}
	// Shared canonical path reuses the immutable snapshot, including symlink aliases.
	link := filepath.Join(t.TempDir(), "alias.ics")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	p.Holidays = &Holidays{Dataset: datasetPath(link)}
	cfg.SLADefinitions[0].SLAs["alias"] = p
	shared := datasetEngine(t, cfg)
	if shared.policies[ref].dataset != shared.policies[PolicyRef{"sla_id", "alias"}].dataset {
		t.Fatal("dataset not cached")
	}
	saveDataset(t, path, icsCalendar(icsEvent("20261225", ""), icsEvent("20260101", ""), icsEvent("20260101", "")), 2020, 2040)
	same := datasetEngine(t, cfg)
	if first.policies[ref].version != same.policies[ref].version {
		t.Fatal("byte order or duplicates changed semantic policy")
	}
	saveDataset(t, path, icsCalendar(icsEvent("20260102", "")), 2020, 2040)
	changed := datasetEngine(t, cfg)
	if changed.policies[ref].version == first.policies[ref].version || changed.policies[otherRef].version != first.policies[otherRef].version {
		t.Fatal("version isolation failed")
	}
	if !first.policies[ref].dataset.dates["2026-01-01"] {
		t.Fatal("on-disk edit mutated running snapshot")
	}
	saveDataset(t, path, contents, 2020, 2041)
	extended := datasetEngine(t, cfg)
	if extended.policies[ref].version == first.policies[ref].version {
		t.Fatal("coverage absent from semantic version")
	}
	// A path-only change with identical effective data is not a calendar change.
	p = cfg.SLADefinitions[0].SLAs["business"]
	p.Holidays = &Holidays{Dataset: datasetPath(other)}
	cfg.SLADefinitions[0].SLAs["business"] = p
	moved := datasetEngine(t, cfg)
	if moved.policies[ref].version != first.policies[ref].version {
		t.Fatal("path affected policy semantics")
	}
}

func TestDatasetMetricsBoundedAndLegacyVersion(t *testing.T) {
	cfg := datasetConfig(t, fixtureDataset(t, "holidays_se.ics"))
	m := newMetrics(cfg)
	engine, err := newCalendarEngineAt(cfg, observedHolidays{provider: CountryHolidays{}, metrics: m}, instant(t, "2026-11-17T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"2026-01-01T12:00:00Z", "2026-11-11T12:00:00Z", "2041-01-01T12:00:00Z"} {
		_, _ = engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, at))
	}
	families, _ := m.registry.Gather()
	results := map[string]bool{}
	for _, family := range families {
		if family.GetName() == "sla_proxy_holiday_evaluations_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "country" && label.GetValue() != "dataset" {
						t.Fatal("unbounded dataset label", label)
					}
					if label.GetName() == "result" {
						results[label.GetValue()] = true
					}
				}
			}
		}
	}
	if len(results) != 3 {
		t.Fatal(results)
	}
	// Freeze the original legacy serialization contract, including field order.
	legacy := testConfig(t)
	compiled, err := NewCalendarEngine(legacy, CountryHolidays{})
	if err != nil {
		t.Fatal(err)
	}
	for ref, p := range compiled.policies {
		policy := legacy.SLADefinitions[0].SLAs[ref.SLAValue]
		for _, def := range legacy.SLADefinitions {
			if def.SLALabel == ref.SLALabel {
				policy = def.SLAs[ref.SLAValue]
			}
		}
		type legacyHolidays struct {
			Country, Subdivision string
			AdditionalDates      []string
		}
		type legacyPolicy struct {
			Timezone string
			Schedule map[string][]string
			Holidays *legacyHolidays
		}
		original := legacyPolicy{Timezone: policy.Timezone, Schedule: policy.Schedule}
		if h := policy.Holidays; h != nil {
			original.Holidays = &legacyHolidays{h.Country, h.Subdivision, h.AdditionalDates}
		}
		data, _ := json.Marshal(struct {
			Ref             PolicyRef
			Policy          legacyPolicy
			HorizonDays     int
			Provider, Rules string
		}{ref, original, legacy.EvaluationHorizonDays, CountryHolidays{}.Version(), "wall-clock-v1"})
		sum := sha256.Sum256(data)
		if p.version != fmt.Sprintf("%x", sum) {
			t.Fatal("legacy policy version changed")
		}
	}
}

func FuzzHolidayDatasetICS(f *testing.F) {
	f.Add([]byte(icsCalendar(icsEvent("20260101", "20260103"))))
	f.Add([]byte("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nEND:VEVENT\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Keep mutation work bounded independently of libFuzzer's input growth.
		if len(data) > maxDatasetLineBytes*2 {
			return
		}
		_, _ = parseHolidayICS(data, 2020, 2040)
	})
}

func TestDatasetWindowsAndHorizonPreserveCalendarRules(t *testing.T) {
	path := writeDataset(t, icsCalendar(icsEvent("20260101", "")), 2020, 2040)
	cfg := datasetConfig(t, path)
	p := cfg.SLADefinitions[0].SLAs["business"]
	p.Timezone = "Europe/Stockholm"
	p.Schedule = map[string][]string{"sunday": {"02:00-03:00"}, "monday": {"08:00-17:00"}}
	cfg.SLADefinitions[0].SLAs["business"] = p
	e := datasetEngine(t, cfg)
	for _, tc := range []struct {
		at       string
		eligible bool
	}{
		{"2026-10-25T02:30:00+02:00", true}, {"2026-10-25T02:30:00+01:00", true},
		{"2026-03-29T01:59:59+01:00", false}, // Spring's 02:00–03:00 window has no real instants.
		{"2026-01-05T08:00:00+01:00", true}, {"2026-01-05T17:00:00+01:00", false},
	} {
		d, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, tc.at))
		if err != nil || d.Eligible != tc.eligible {
			t.Fatal(tc, d, err)
		}
		if strings.HasPrefix(tc.at, "2026-03") && !d.NextEvaluationAt.Equal(instant(t, "2026-03-30T08:00:00+02:00")) {
			t.Fatal("spring gap normalized", d)
		}
	}
	cfg.EvaluationHorizonDays = 1
	p.Schedule = map[string][]string{"monday": {}}
	cfg.SLADefinitions[0].SLAs["business"] = p
	e = datasetEngine(t, cfg)
	d, err := e.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", instant(t, "2027-11-17T12:00:00Z"))
	if err != nil || d.Reason != ReasonHorizonExhausted || d.SearchUntil.IsZero() {
		t.Fatal("covered empty year was treated as missing data", d, err)
	}
}

func TestDatasetManifestProvenanceDoesNotChangeSemantics(t *testing.T) {
	path := writeDataset(t, icsCalendar(icsEvent("20260101", "")), 2020, 2040)
	cfg := datasetConfig(t, path)
	original := datasetEngine(t, cfg).policies[PolicyRef{"sla_id", "business"}].version
	data, _ := os.ReadFile(path + ".json")
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	m["country"] = "FR"
	m["subdivision"] = "informational-only"
	m["source_version"] = "different"
	data, _ = json.Marshal(m)
	if err := os.WriteFile(path+".json", data, 0600); err != nil {
		t.Fatal(err)
	}
	changed := datasetEngine(t, cfg).policies[PolicyRef{"sla_id", "business"}].version
	if changed != original {
		t.Fatal("provenance selected rules or changed policy semantics")
	}
	// encoding/json alone would replace invalid UTF-8; the manifest loader rejects it.
	if _, err := parseDatasetManifest([]byte("{\"source\":\"\xff\"}")); err == nil {
		t.Fatal("invalid UTF-8 manifest accepted")
	}
}
