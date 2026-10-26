package engine

// Problem kinds (state.Problem.Kind): what sync can't resolve by itself
// and shows in «Проблемы и конфликты» (cloudsync problems).
const (
	// Found by the planner on each pass and dropped once gone.
	ProblemNameInvalid   = "name_invalid"   // the server refuses the local name
	ProblemNameWindows   = "name_windows"   // a server name Windows can't store
	ProblemNameIgnored   = "name_ignored"   // a server name this client ignores locally
	ProblemCaseCollision = "case_collision" // two local names the server takes for one
	ProblemReadOnly      = "readonly"       // a local change in a folder without write access
	ProblemUnselectKept  = "unselect_kept"  // an unchecked folder's edit that couldn't be uploaded

	// Recorded by the engine as they happen.
	ProblemConflict    = "conflict"     // a conflict copy was made (kept until dismissed)
	ProblemRescued     = "rescued"      // an unsynced change that can't go up where it was (a share gone), moved to the own tree
	ProblemMassDelete  = "mass_delete"  // deletes paused, waiting for confirmation
	ProblemQuota       = "quota"        // the server is out of quota for an upload
	ProblemLocked      = "locked"       // locked on the server for over LockRetryFor
	ProblemForbidden   = "forbidden"    // the server refused an action
	ProblemFailing     = "failing"      // an op keeps failing
	ProblemNeedsSignIn = "needs_signin" // the device token was revoked
	ProblemNoMarker    = "no_marker"    // the sync folder's .cloudsync is gone
	ProblemOffline     = "offline"      // the server can't be reached (no network, or it is down)
)

// plannerProblems are the kinds a pass recomputes: one not found again is
// resolved.
var plannerProblems = []string{
	ProblemNameInvalid, ProblemNameWindows, ProblemNameIgnored, ProblemCaseCollision,
	ProblemReadOnly, ProblemUnselectKept,
}
