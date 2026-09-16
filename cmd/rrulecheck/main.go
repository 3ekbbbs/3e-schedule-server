// Throwaway diagnostic: does the RRULE expansion terminate for the rule the
// smoke test uses? Run with a wall-clock guard so an infinite iterator shows
// up as a timeout instead of a hung shell.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/teambition/rrule-go"
)

func main() {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		fmt.Println("tz error:", err)
		os.Exit(1)
	}

	now := time.Now()
	base := time.Unix(now.Unix()-(now.Unix()%86400)+9*3600+86400, 0)
	from := now.Add(-7 * 24 * time.Hour)
	to := now.Add(90 * 24 * time.Hour)

	fmt.Println("dtstart(raw):", base.UTC())
	fmt.Println("dtstart(loc):", base.In(loc))
	fmt.Println("from(loc)   :", from.In(loc))
	fmt.Println("to(loc)     :", to.In(loc))

	for _, spec := range []string{
		"FREQ=WEEKLY;BYDAY=MO,WE;COUNT=8",
		"FREQ=DAILY",
		"FREQ=MONTHLY;BYMONTHDAY=31",
		"FREQ=YEARLY",
	} {
		done := make(chan int, 1)
		go func(spec string) {
			opt, err := rrule.StrToROptionInLocation(spec, loc)
			if err != nil {
				fmt.Printf("%-34s parse error: %v\n", spec, err)
				done <- -1
				return
			}
			opt.Dtstart = base.In(loc)
			r, err := rrule.NewRRule(*opt)
			if err != nil {
				fmt.Printf("%-34s newrrule error: %v\n", spec, err)
				done <- -1
				return
			}
			set := &rrule.Set{}
			set.DTStart(base.In(loc))
			set.RRule(r)
			res := set.Between(from.In(loc), to.In(loc), true)
			fmt.Printf("%-34s -> %d occurrences, first=%v\n", spec, len(res), firstOf(res))
			done <- len(res)
		}(spec)

		select {
		case n := <-done:
			_ = n
		case <-time.After(4 * time.Second):
			fmt.Printf("%-34s >>> HANG (infinite iterator)\n", spec)
			os.Exit(2)
		}
	}
	fmt.Println("all rules terminated OK")
}

func firstOf(ts []time.Time) any {
	if len(ts) == 0 {
		return nil
	}
	return ts[0].UTC()
}
