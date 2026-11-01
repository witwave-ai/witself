package localcell // FLAKE-3 fix 3 (F7)
import "time"     // FLAKE-3 fix 3 (F7)
// EffectiveTimeoutForTest exposes the timeout retained by New. // FLAKE-3 fix 3 (F7)
func (c *Cell) EffectiveTimeoutForTest() time.Duration { return c.cfg.Timeout } // FLAKE-3 fix 3 (F7)
