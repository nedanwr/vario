package claudecode

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// Transcript fields are decoded leniently: a value of an unexpected type (as
// different Claude Code versions may write) is treated as absent instead of
// making the whole line unreadable.

type lstr string

func (s *lstr) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = lstr(v)
	}
	return nil
}

type lbool bool

func (s *lbool) UnmarshalJSON(b []byte) error {
	var v bool
	if json.Unmarshal(b, &v) == nil {
		*s = lbool(v)
	}
	return nil
}

// lnum is a non-negative integer, or absent.
type lnum struct {
	v  uint64
	ok bool
}

func (n *lnum) UnmarshalJSON(b []byte) error {
	s := string(bytes.TrimSpace(b))
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		*n = lnum{v, true}
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f == math.Trunc(f) && f < math.MaxUint64 {
		*n = lnum{uint64(f), true}
	}
	return nil
}

func (n lnum) ptr() *uint64 {
	if !n.ok {
		return nil
	}
	v := n.v
	return &v
}

// lint is an integer (possibly negative or fractional, truncated), or absent.
type lint struct {
	v  int64
	ok bool
}

func (n *lint) UnmarshalJSON(b []byte) error {
	var f float64
	if json.Unmarshal(b, &f) == nil && !math.IsNaN(f) && math.Abs(f) < math.MaxInt64 {
		*n = lint{int64(f), true}
	}
	return nil
}
