package slaproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	ics "github.com/arran4/golang-ical"
)

var errDatasetCoverage = errors.New("ICS holiday dataset does not cover the requested local date")

const (
	maxDatasetBytes     = 8 << 20
	maxManifestBytes    = 64 << 10
	maxDatasetEvents    = 100_000
	maxDatasetDates     = 200_000 // Total expansion work, including duplicate dates.
	maxEventDays        = 3660
	maxDatasetLineBytes = 64 << 10 // Unfolded content line.
	datasetRulesVersion = "ics-civil-dates-v1"
)

type datasetManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Format        string `json:"format"`
	Country       string `json:"country"`
	Subdivision   string `json:"subdivision"`
	FirstYear     int    `json:"first_year"`
	LastYear      int    `json:"last_year"`
	Source        string `json:"source"`
	SourceVersion string `json:"source_version"`
	ICSSHA256     string `json:"ics_sha256"`
}

// Immutable after compilation. Dates are civil keys, never UTC-converted instants.
type holidayDataset struct {
	dates               map[string]bool
	firstYear, lastYear int
	semanticHash        string
}

func (d *holidayDataset) covers(date time.Time) bool {
	return date.Year() >= d.firstYear && date.Year() <= d.lastYear
}

// Stat before opening rejects special files without reading them. O_NONBLOCK
// also prevents an operator racing a regular file with a FIFO from hanging startup.
func readDatasetFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("dataset files must be regular and within the size limit")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("dataset files must be regular and within the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > maximum {
		return nil, errors.New("empty or oversized dataset file")
	}
	return data, nil
}

func loadHolidayDataset(path string) (*holidayDataset, error) {
	data, err := readDatasetFile(path, maxDatasetBytes)
	if err != nil {
		return nil, fmt.Errorf("read ICS dataset: %w", err)
	}
	metadata, err := readDatasetFile(path+".json", maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read adjacent ICS manifest: %w", err)
	}
	manifest, err := parseDatasetManifest(metadata)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != manifest.ICSSHA256 {
		return nil, errors.New("ICS SHA-256 does not match manifest")
	}
	dates, err := parseHolidayICS(data, manifest.FirstYear, manifest.LastYear)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(dates))
	for date := range dates {
		keys = append(keys, date)
	}
	sort.Strings(keys)
	encoded, _ := json.Marshal(struct {
		Rules               string
		FirstYear, LastYear int
		Dates               []string
	}{datasetRulesVersion, manifest.FirstYear, manifest.LastYear, keys})
	sum = sha256.Sum256(encoded)
	return &holidayDataset{dates: dates, firstYear: manifest.FirstYear, lastYear: manifest.LastYear, semanticHash: hex.EncodeToString(sum[:])}, nil
}

func parseDatasetManifest(data []byte) (datasetManifest, error) {
	var manifest datasetManifest
	if len(data) == 0 || len(data) > maxManifestBytes || !utf8.Valid(data) {
		return manifest, errors.New("manifest must be bounded UTF-8 JSON")
	}
	// This manifest is flat; reject nesting before duplicate-key validation so
	// deeply nested untrusted JSON cannot consume recursive validation resources.
	keys := json.NewDecoder(bytes.NewReader(data))
	if token, err := keys.Token(); err != nil || token != json.Delim('{') {
		return manifest, errors.New("manifest must be a JSON object")
	}
	seen := map[string]bool{}
	for keys.More() {
		token, err := keys.Token()
		if err != nil {
			return manifest, errors.New("invalid manifest JSON")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return manifest, errors.New("duplicate manifest key")
		}
		seen[key] = true
		token, err = keys.Token()
		if err != nil {
			return manifest, errors.New("invalid manifest value")
		}
		if _, nested := token.(json.Delim); nested || token == nil {
			return manifest, errors.New("manifest values must be non-null scalars")
		}
	}
	if token, err := keys.Token(); err != nil || token != json.Delim('}') {
		return manifest, errors.New("invalid manifest object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return manifest, errors.New("invalid or unknown manifest field")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return manifest, errors.New("manifest must contain one JSON document")
	}
	if manifest.SchemaVersion != 1 || manifest.Format != "ics-holiday-dataset" || manifest.FirstYear < 1 || manifest.LastYear > 9999 || manifest.FirstYear > manifest.LastYear || manifest.Source == "" || manifest.SourceVersion == "" {
		return manifest, errors.New("invalid manifest schema, coverage or provenance")
	}
	digest, err := hex.DecodeString(manifest.ICSSHA256)
	if err != nil || len(digest) != sha256.Size || strings.ToLower(manifest.ICSSHA256) != manifest.ICSSHA256 {
		return manifest, errors.New("manifest requires lowercase ICS SHA-256")
	}
	return manifest, nil
}

// The library performs RFC content-line unfolding/property parsing. This guard
// only enforces the restricted component envelope and bounds before its parser
// allocates a component tree; notably the upstream calendar parser accepts EOF
// before END:VCALENDAR. It is not a second RFC property/recurrence parser.
func validateDatasetEnvelope(data []byte) error {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return errors.New("ICS must be UTF-8 text without NUL")
	}
	stream := ics.NewCalendarStream(bytes.NewReader(data))
	state := "before"
	events := 0
	for {
		line, _, err := stream.ReadLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return errors.New("invalid ICS content line")
		}
		if line != nil && len(*line) > 0 {
			if len(*line) > maxDatasetLineBytes {
				return errors.New("ICS content line exceeds limit")
			}
			text := strings.ToUpper(string(*line))
			switch text {
			case "BEGIN:VCALENDAR":
				if state != "before" {
					return errors.New("ICS must contain exactly one calendar")
				}
				state = "calendar"
			case "BEGIN:VEVENT":
				if state != "calendar" {
					return errors.New("unsupported nested ICS component")
				}
				state = "event"
				events++
				if events > maxDatasetEvents {
					return errors.New("ICS event count exceeds limit")
				}
			case "END:VEVENT":
				if state != "event" {
					return errors.New("unbalanced ICS event")
				}
				state = "calendar"
			case "END:VCALENDAR":
				if state != "calendar" {
					return errors.New("unbalanced ICS calendar")
				}
				state = "after"
			default:
				if state == "before" || state == "after" || strings.HasPrefix(text, "BEGIN:") || strings.HasPrefix(text, "END:") || strings.HasPrefix(text, "BEGIN;") || strings.HasPrefix(text, "END;") {
					return errors.New("unsupported ICS component or trailing content")
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if state != "after" || events == 0 {
		return errors.New("ICS requires a complete calendar and at least one event")
	}
	return nil
}

func datasetDate(property *ics.IANAProperty) (time.Time, error) {
	if len(property.ICalParameters) != 1 {
		return time.Time{}, errors.New("holiday dates require only VALUE=DATE")
	}
	value := property.ICalParameters["VALUE"]
	if len(value) != 1 || value[0] != "DATE" {
		return time.Time{}, errors.New("holiday dates require VALUE=DATE")
	}
	date, err := time.Parse("20060102", property.Value)
	if err != nil || date.Year() < 1 || date.Format("20060102") != property.Value {
		return time.Time{}, errors.New("invalid ICS civil date")
	}
	return date, nil
}

func parseHolidayICS(data []byte, firstYear, lastYear int) (map[string]bool, error) {
	if len(data) > maxDatasetBytes {
		return nil, errors.New("ICS file exceeds size limit")
	}
	if err := validateDatasetEnvelope(data); err != nil {
		return nil, err
	}
	calendar, err := ics.ParseCalendar(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid ICS syntax")
	}
	versions := 0
	for _, p := range calendar.CalendarProperties {
		switch strings.ToUpper(p.IANAToken) {
		case "VERSION":
			versions++
			if p.Value != "2.0" {
				return nil, errors.New("ICS version must be 2.0")
			}
		case "RRULE", "RDATE", "EXDATE", "EXRULE", "RECURRENCE-ID", "DTSTART", "DTEND", "DURATION":
			return nil, errors.New("unsupported calendar-level temporal property")
		case "METHOD":
			if p.Value != "PUBLISH" {
				return nil, errors.New("unsupported ICS method")
			}
		case "CALSCALE":
			if p.Value != "GREGORIAN" {
				return nil, errors.New("unsupported ICS calendar scale")
			}
		}
	}
	if versions != 1 {
		return nil, errors.New("ICS requires exactly one VERSION:2.0")
	}
	dates := map[string]bool{}
	expanded := 0
	for _, component := range calendar.Components {
		event, ok := component.(*ics.VEvent)
		if !ok || len(event.SubComponents()) != 0 {
			return nil, errors.New("ICS holidays require VEVENT without subcomponents")
		}
		var start, end *ics.IANAProperty
		for i := range event.Properties {
			p := &event.Properties[i]
			switch strings.ToUpper(p.IANAToken) {
			case "DTSTART":
				if start != nil {
					return nil, errors.New("duplicate DTSTART")
				}
				start = p
			case "DTEND":
				if end != nil {
					return nil, errors.New("duplicate DTEND")
				}
				end = p
			case "DURATION", "RRULE", "RDATE", "EXDATE", "EXRULE", "RECURRENCE-ID":
				return nil, errors.New("unsupported ICS temporal property; export explicit DTSTART/DTEND dates")
			case "STATUS":
				if strings.EqualFold(p.Value, "CANCELLED") {
					return nil, errors.New("cancelled holiday event is unsupported")
				}
			}
		}
		if start == nil {
			return nil, errors.New("holiday event requires DTSTART")
		}
		from, err := datasetDate(start)
		if err != nil {
			return nil, err
		}
		until := from.AddDate(0, 0, 1)
		if end != nil {
			until, err = datasetDate(end)
			if err != nil {
				return nil, err
			}
		}
		days := int(until.Sub(from) / (24 * time.Hour))
		if days < 1 || days > maxEventDays {
			return nil, errors.New("invalid or excessive holiday date range")
		}
		expanded += days
		if expanded > maxDatasetDates {
			return nil, errors.New("ICS date expansion exceeds limit")
		}
		for date := from; date.Before(until); date = date.AddDate(0, 0, 1) {
			if date.Year() < firstYear || date.Year() > lastYear {
				return nil, errors.New("holiday event outside declared coverage")
			}
			dates[date.Format("2006-01-02")] = true
		}
	}
	return dates, nil
}

func canonicalDatasetPath(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve ICS dataset path: %w", err)
	}
	return canonical, nil
}
