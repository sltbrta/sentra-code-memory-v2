// Package meetingapi implements the five frozen MeetingService methods behind
// injected ports on the authenticated local authority gateway. It holds no
// state between calls; the composed meeting kernel owns durability and all
// meeting authority. Live capture remains deferred (DEF-002).
package meetingapi
