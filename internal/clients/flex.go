package clients

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// FlexInt decodes a JSON number, numeric string, empty string or null.
//
// Tautulli in particular returns the same field as an int on one row and a
// string on the next, and uses "" for "unknown". Valid is false for anything
// that does not carry a number, so "never played" stays distinct from zero.
type FlexInt struct {
	Value int64
	Valid bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	*f = FlexInt{}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}

	s := string(data)
	if data[0] == '"' {
		if err := json.Unmarshal(data, &s); err != nil {
			return nil
		}
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return nil
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		f.Value, f.Valid = n, true
		return nil
	}
	if fl, err := strconv.ParseFloat(s, 64); err == nil {
		f.Value, f.Valid = int64(fl), true
	}
	return nil
}

// Ptr returns a pointer to the value, or nil when absent.
func (f FlexInt) Ptr() *int64 {
	if !f.Valid {
		return nil
	}
	v := f.Value
	return &v
}

// Time interprets the value as a Unix timestamp. Zero and negative values are
// treated as absent: Plex and Tautulli both use 0 for "never".
func (f FlexInt) Time() *time.Time {
	if !f.Valid || f.Value <= 0 {
		return nil
	}
	t := time.Unix(f.Value, 0).UTC()
	return &t
}

// FlexString decodes a JSON string or number into a string.
type FlexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexString) UnmarshalJSON(data []byte) error {
	*f = ""
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return nil
		}
		*f = FlexString(s)
		return nil
	}
	*f = FlexString(string(data))
	return nil
}
