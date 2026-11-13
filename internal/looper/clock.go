package looper

import "time"

// wallClock is the clock every recorded JSON document reads. Production keeps
// the real clock; the byte-fixture test substitutes a fixed one so the
// documents compare byte for byte against
// docs/contracts/fixtures/legacy/*.bytes.json.
var wallClock = time.Now
