package slaproxy

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestQueueScoreCeilsOnlyFractionalMilliseconds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    time.Time
		score float64
	}{
		{"aligned", time.Unix(100, 123000000), 100123},
		{"one_nanosecond", time.Unix(100, 123000001), 100124},
		{"before_boundary", time.Unix(100, 123999999), 100124},
		{"next_boundary", time.Unix(100, 124000000), 100124},
		{"before_epoch", time.Unix(-1, 999999999), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Building a go-redis pipeline does not require a server connection.
			client := redis.NewClient(&redis.Options{Addr: "unused:0"})
			t.Cleanup(func() { _ = client.Close() })
			p := client.Pipeline()
			queueScore(context.Background(), p, "due", "id", tc.at)
			commands := p.Len()
			if commands != 1 {
				t.Fatal(commands)
			}
			if got := deadlineScore(tc.at); got != tc.score {
				t.Fatal(got, tc.score)
			}
			deadline := time.UnixMilli(int64(deadlineScore(tc.at)))
			if deadline.Before(tc.at) || deadline.Sub(tc.at) >= time.Millisecond {
				t.Fatal("invalid rounding", deadline, tc.at)
			}
			p.Discard()
		})
	}
}
