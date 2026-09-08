package store

import (
	"errors"
	"time"
)

var errAlreadyClaimed = errors.New("already claimed")

func timeoutCh() <-chan time.Time { return time.After(10 * time.Second) }
