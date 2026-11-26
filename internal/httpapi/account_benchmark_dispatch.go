package httpapi

// These handlers remain behind the account session and the opt-in instance
// switch. They operate only on the caller's personal plans and tasks; none of
// them can grant permission to a physical asset or change the fixed provider.
func accountBenchmarkDispatchRoute(route string) bool {
	switch route {
	case "POST /api/v1/domain/benchmarks",
		"GET /api/v1/domain/benchmark-profiles", "POST /api/v1/domain/benchmark-profiles",
		"GET /api/v1/domain/benchmark-profiles/{key}/versions", "GET /api/v1/domain/benchmark-profiles/{key}/versions/{version}",
		"POST /api/v1/domain/benchmark-preflight",
		"GET /api/v1/domain/benchmark-chat-targets/{target}", "PUT /api/v1/domain/benchmark-chat-targets/{target}", "DELETE /api/v1/domain/benchmark-chat-targets/{target}",
		"POST /api/v1/domain/benchmark-admission/preview", "POST /api/v1/domain/benchmark-admission/preview/{id}/confirm",
		"POST /api/v1/conversations/{conversation}/benchmark-suggestions/{run}/preview",
		"GET /api/v1/conversations/{id}/a2ui/benchmark-admission/{run}", "POST /api/v1/conversations/{id}/a2ui/benchmark-admission/{run}",
		"POST /api/v1/domain/benchmarks/{id}/cases/{step}/execution-control":
		return true
	default:
		return false
	}
}
