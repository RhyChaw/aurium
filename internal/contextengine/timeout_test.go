package contextengine

import "time"

func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }
