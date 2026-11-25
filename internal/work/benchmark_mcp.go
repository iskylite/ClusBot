package work

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	benchmarkIndexList = regexp.MustCompile(`^[0-9]+(?:-[0-9]+)?(?:,[0-9]+(?:-[0-9]+)?)*$`)
	benchmarkNodeName  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)
)

func cluskitBenchmarkArguments(req BenchmarkJobRequest) (map[string]any, error) {
	bad := func(key string) (map[string]any, error) {
		return nil, fmt.Errorf("%w：Cluskit2 基准参数 %q 无效或当前工具不支持", ErrBenchmarkInput, key)
	}
	if len(req.Targets) == 0 {
		return bad("targets")
	}
	for _, target := range req.Targets {
		// Domain refs describe approved exact targets, not a task-specific
		// expanded nodeset, login, inventory path or alternate port.
		if target == "" || !benchmarkNodeName.MatchString(target) && net.ParseIP(target) == nil || strings.HasSuffix(target, "..") {
			return bad("targets")
		}
	}
	args := map[string]any{"idempotency_key": req.IdempotencyKey, "nodes": strings.Join(req.Targets, ",")}
	for key, value := range req.Parameters {
		if value == "" || strings.TrimSpace(value) != value {
			return bad(key)
		}
		switch key {
		case "suite":
			if !benchmarkNodeName.MatchString(value) {
				return bad(key)
			}
			args[key] = value
		case "benchmark", "alg", "template", "exclude":
			args[key] = value
		case "mode", "protocol", "ssh_backend", "device":
			values := map[string]string{"mode": "single mp separate osu", "protocol": "ssh tree", "ssh_backend": "system internal", "device": "cpu dcu gpu"}
			if !strings.Contains(" "+values[key]+" ", " "+value+" ") || strings.Contains(value, " ") {
				return bad(key)
			}
			args[key] = value
		case "timeout":
			if d, err := time.ParseDuration(value); err != nil || d <= 0 {
				return bad(key)
			}
			args[key] = value
		case "numa", "dcu":
			if value != "all" && !benchmarkIndexList.MatchString(value) {
				return bad(key)
			}
			args[key] = value
		case "percent":
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || !finiteBenchmark(n) || n <= 0 || n > 100 {
				return bad(key)
			}
			args[key] = n
		case "fanout", "concurrency", "group_size", "iterations", "ibperf_runtime":
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil || n <= 0 || key == "concurrency" && req.Parameters["fanout"] != "" {
				return bad(key)
			}
			if key == "concurrency" {
				key = "fanout"
			}
			args[key] = int(n)
		case "enable_nhc", "prepare_only":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return bad(key)
			}
			args[key] = b
		case "ibperf_hcas", "args", "baseline", "bench_configs":
			var list []string
			if json.Unmarshal([]byte(value), &list) != nil || len(list) == 0 || len(list) > 32 {
				return bad(key)
			}
			for _, v := range list {
				if v == "" || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\x00") {
					return bad(key)
				}
				if key == "args" {
					k, val, ok := strings.Cut(v, ":")
					if !ok || !validBenchKey(k) || val == "" {
						return bad(key)
					}
				}
			}
			args[key] = list
		default:
			// In particular, freeze targets separately: hostfile and nodes
			// must not expand outside the scope of the approved case.
			return bad(key)
		}
	}
	selectors := 0
	for _, key := range []string{"benchmark", "suite", "bench_configs"} {
		if _, ok := args[key]; ok {
			selectors++
		}
	}
	if selectors > 1 {
		return bad("benchmark/suite/bench_configs")
	}
	if selectors == 0 {
		args["benchmark"] = req.Case
	}
	mode, _ := args["mode"].(string)
	benchmark, _ := args["benchmark"].(string)
	if _, set := args["benchmark"]; set && !cluskitBenchmarkName(benchmark) {
		return bad("benchmark")
	}
	if mode == "osu" && benchmark != "" && !strings.HasPrefix(benchmark, "osu:") &&
		benchmark != "bw" && benchmark != "bibw" && benchmark != "latency" && benchmark != "alltoall" {
		return bad("mode")
	}
	if args["group_size"] != nil && mode != "mp" || args["prepare_only"] == true && mode != "mp" && mode != "separate" && mode != "osu" {
		return bad("mode")
	}
	if selectors == 1 && (args["suite"] != nil || args["bench_configs"] != nil) && (mode == "mp" || mode == "separate") {
		return bad("mode")
	}
	if (args["ibperf_hcas"] != nil || args["ibperf_runtime"] != nil) && !strings.EqualFold(benchmark, "ibperf") {
		return bad("ibperf")
	}
	return args, nil
}

func cluskitBenchmarkName(name string) bool {
	switch strings.ToLower(name) {
	case "hpl", "hplmxp", "hpcg", "stream", "dgemm", "sgemm", "hgemm", "tf32gemm", "bf16gemm", "int8gemm", "fp8gemm", "rocm-bandwidth-test", "bw", "bibw", "latency", "alltoall", "hsl", "ibperf":
		return true
	}
	kind, operation, found := strings.Cut(strings.ToLower(name), ":")
	if !found {
		return false
	}
	switch kind {
	case "rccl":
		return allowedCluskitBenchmarkToken(operation, "all_reduce_perf all_gather_perf alltoall_perf alltoallv_perf broadcast_perf gather_perf reduce_perf reduce_scatter_perf scatter_perf sendrecv_perf")
	case "osu":
		operation = strings.TrimPrefix(operation, "osu_")
		return allowedCluskitBenchmarkToken(operation, "bw bibw mbw_mr latency allgather allgatherv allreduce alltoall alltoallv alltoallw barrier bcast gather gatherv iallgather iallgatherv iallreduce ialltoall ialltoallv ialltoallw ibarrier ibcast igather igatherv ineighbor_allgather ineighbor_allgatherv ineighbor_alltoall ineighbor_alltoallv ineighbor_alltoallw ireduce ireduce_scatter ireduce_scatter_block iscatter iscatterv neighbor_allgather neighbor_allgatherv neighbor_alltoall neighbor_alltoallv neighbor_alltoallw reduce reduce_scatter reduce_scatter_block scatter scatterv")
	}
	return false
}

func allowedCluskitBenchmarkToken(value, tokens string) bool {
	return value != "" && !strings.Contains(value, " ") && strings.Contains(" "+tokens+" ", " "+value+" ")
}

func cluskitBenchmarkResultMatches(requested, observed string) bool {
	req, row := strings.ToLower(requested), strings.ToLower(observed)
	if req == row {
		return true
	}
	switch kind, arg, found := strings.Cut(req, ":"); {
	case found && kind == "rccl":
		return row == arg || row == arg+"_card" || row == arg+"_mp"
	case found && kind == "osu":
		op := strings.TrimPrefix(arg, "osu_")
		return row == "osu_"+op || row == "dcu_osu_"+op || row == "osu_"+op+"_mp" || row == "dcu_osu_"+op+"_mp"
	}
	return row == "dcu_"+req || row == "dcu_"+req+"_card" || row == "dcu_"+req+"_node" ||
		row == "cpu_"+req || row == "cpu_"+req+"_mp"
}

type cluskitBenchmarkResultRecord struct {
	TaskID string `json:"task_id"`
	MainID string `json:"main_id"`
	Node   string `json:"node"`
	Event  string `json:"event"`
	State  string `json:"state"`
	Code   int    `json:"code"`
	Data   struct {
		Benchmark    string                     `json:"benchmark"`
		ActualConfig map[string]string          `json:"actual_config"`
		Raw          *BenchmarkRawArtifact      `json:"raw"`
		Nodes        []string                   `json:"nodes"`
		Metrics      map[string]json.RawMessage `json:"metrics"`
		Row          struct {
			Node         string            `json:"node"`
			Benchmark    string            `json:"benchmark"`
			Status       string            `json:"status"`
			ActualConfig map[string]string `json:"actual_config"`
			Metrics      struct {
				State  string             `json:"state"`
				Unit   string             `json:"unit"`
				Fields map[string]float64 `json:"fields"`
			} `json:"metrics"`
		} `json:"row"`
	} `json:"data"`
}

func cluskitBenchmarkMeasurements(req BenchmarkJobRequest, job string, records []cluskitBenchmarkResultRecord) []BenchmarkMeasurement {
	var out []BenchmarkMeasurement
	name := firstNonEmpty(req.Parameters["benchmark"], req.Case)
	allowed := make(map[string]bool, len(req.Targets))
	for _, target := range req.Targets {
		allowed[target] = true
	}
	for _, rec := range records {
		nameFromRow := firstNonEmpty(rec.Data.Row.Benchmark, rec.Data.Benchmark)
		if rec.Event != "result" || rec.TaskID != job && rec.MainID != job || rec.MainID != "" && rec.MainID != job ||
			nameFromRow == "" || req.Parameters["suite"] == "" && req.Parameters["bench_configs"] == "" && !cluskitBenchmarkResultMatches(name, nameFromRow) ||
			rec.Data.Row.Benchmark != "" && rec.Data.Benchmark != "" && !strings.EqualFold(rec.Data.Row.Benchmark, rec.Data.Benchmark) {
			continue
		}
		// A group row represents a group measurement, not a separate measured
		// number for each participant. Do not duplicate it across targets.
		if len(rec.Data.Nodes) > 1 || rec.Node != "" && rec.Data.Row.Node != "" && rec.Node != rec.Data.Row.Node {
			continue
		}
		node := firstNonEmpty(rec.Data.Row.Node, rec.Node)
		if !allowed[node] {
			continue
		}
		outcome := "unknown"
		switch {
		case strings.EqualFold(rec.State, "skipped") || strings.EqualFold(rec.Data.Row.Status, "skipped"):
			outcome = "skipped"
		case rec.Code != 0 || strings.EqualFold(rec.State, "failed") || strings.EqualFold(rec.Data.Row.Status, "failed") || strings.EqualFold(rec.Data.Row.Metrics.State, "failed"):
			outcome = "failed"
		case strings.EqualFold(rec.State, "succeeded") && (rec.Data.Row.Status == "" || strings.EqualFold(rec.Data.Row.Status, "succeeded")):
			outcome = "measured"
		}
		unit := rec.Data.Row.Metrics.Unit
		if unit == "" {
			_ = json.Unmarshal(rec.Data.Metrics["Unit"], &unit)
		}
		fields := rec.Data.Row.Metrics.Fields
		if len(fields) == 0 {
			fields = make(map[string]float64, len(rec.Data.Metrics))
			for key, raw := range rec.Data.Metrics {
				var numeric *float64
				if json.Unmarshal(raw, &numeric) == nil && numeric != nil {
					fields[key] = *numeric
				}
			}
		}
		metrics := map[string]BenchmarkMeasuredMetric{}
		for key, value := range fields {
			if metricUnit := cluskitBenchmarkMetricUnit(key, unit); metricUnit != "" && finiteBenchmark(value) {
				metrics[key] = BenchmarkMeasuredMetric{Value: value, Unit: metricUnit}
			}
		}
		actual := rec.Data.ActualConfig
		if len(actual) == 0 {
			actual = rec.Data.Row.ActualConfig
		}
		out = append(out, BenchmarkMeasurement{Target: node, Outcome: outcome, ActualConfig: actual, Metrics: metrics, Raw: rec.Data.Raw})
	}
	return out
}

func cluskitBenchmarkMetricUnit(field, primary string) string {
	switch field {
	case "Rmax", "Rpeak", "Bandwidth", "BW", "BusBW", "AlgBW", "Copy", "Scale", "Add", "Triad", "Dot", "Mul", "Rpeak_bw", "Latency", "Gmean_result":
		return primary
	case "N", "NB", "P", "Q", "NUMA", "DCU":
		return "count"
	case "Time":
		return "s"
	case "Residual", "Efficiency":
		return "ratio"
	}
	return ""
}
