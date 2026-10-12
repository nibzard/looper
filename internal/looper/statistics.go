package looper

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

func addUSD(total **float64, value *float64) {
	if value == nil {
		return
	}
	if *total == nil {
		v := 0.0
		*total = &v
	}
	**total += *value
}

// Statistics reads durable records. Viewers never scan agent logs or depend on
// their bounded activity stream to calculate project totals.
func (s *Store) Statistics(now time.Time) (ui.StatisticsView, error) {
	out := ui.StatisticsView{Version: 1, UpdatedAt: now.UTC().Format(time.RFC3339)}
	timing, err := s.Timing(now)
	if err != nil {
		return out, err
	}
	out.FirstRecorded = timing.FirstRecorded
	out.ElapsedSeconds, out.ActiveSeconds, out.WorkerSeconds, out.ControllerSeconds = timing.ElapsedSeconds, timing.ActiveSeconds, timing.WorkerSeconds, timing.ControllerSeconds
	out.HistoryComplete, out.UnknownEnds = timing.HistoryComplete, timing.UnknownProcesses+timing.UnknownSessions
	state, err := s.Load()
	if err != nil {
		return out, err
	}
	attempts := map[string]bool{}
	if state != nil {
		for _, task := range state.Backlog.Tasks {
			if task.Marker() {
				continue
			}
			out.TasksTotal++
			if task.Status == "done" {
				out.TasksDone++
			}
		}
		for _, attempt := range state.Attempts {
			attempts[attempt.ID] = true
			out.Repairs += attempt.Repairs
		}
	}
	var repairs int
	if err = s.db.QueryRow("SELECT count(*) FROM events WHERE kind='repair'").Scan(&repairs); err != nil {
		return out, err
	}
	out.Repairs = max(out.Repairs, repairs)
	midnight := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	dates := map[string]int{}
	for day := 13; day >= 0; day-- {
		date := midnight.AddDate(0, 0, -day).Format("2006-01-02")
		dates[date] = len(out.Days)
		out.Days = append(out.Days, ui.DailyStatistics{Date: date})
	}
	var usageTables int
	if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='usage_reports'").Scan(&usageTables); err != nil {
		return out, err
	}
	if timing.Available {
		if err = s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&out.Sessions); err != nil {
			return out, err
		}
		rows, err := s.db.Query("SELECT started,ended,pid,identity,outcome,artifact,attempt_id FROM processes")
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var start, identity, outcome, artifact, attemptID string
			var end sql.NullString
			var pid int
			if err = rows.Scan(&start, &end, &pid, &identity, &outcome, &artifact, &attemptID); err != nil {
				rows.Close()
				return out, err
			}
			if attemptID != "" {
				attempts[attemptID] = true
			}
			if filepath.Ext(artifact) == ".jsonl" {
				out.AgentRuns++
			}
			a, e := time.Parse(time.RFC3339Nano, start)
			if e != nil || a.After(now) {
				continue
			}
			b := now
			if end.Valid {
				b, e = time.Parse(time.RFC3339Nano, end.String)
				if e != nil {
					continue
				}
			} else {
				current, e := runIdentityCommand(pid)
				if outcome != "running" || e != nil || identity == "" || current != identity {
					continue
				}
			}
			if b.After(now) {
				b = now
			}
			if a.Before(midnight.AddDate(0, 0, -13)) {
				a = midnight.AddDate(0, 0, -13)
			}
			for a.Before(b) {
				next := time.Date(a.UTC().Year(), a.UTC().Month(), a.UTC().Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
				if next.After(b) {
					next = b
				}
				if index, ok := dates[a.UTC().Format("2006-01-02")]; ok {
					out.Days[index].WorkerSeconds += next.Sub(a).Seconds()
				}
				a = next
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	out.Attempts = len(attempts)
	rows, err := s.db.Query("SELECT time FROM events WHERE kind='result' AND instr(message,'  done | ')>0")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var timestamp string
		if err = rows.Scan(&timestamp); err != nil {
			rows.Close()
			return out, err
		}
		out.Integrations++
		if len(timestamp) >= 10 {
			if index, ok := dates[timestamp[:10]]; ok {
				out.Days[index].Integrations++
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = s.db.QueryRow(`SELECT COALESCE(SUM(result='pass'),0),COALESCE(SUM(result='fail'),0) FROM checks WHERE kind='executable'`).Scan(&out.ChecksPassed, &out.ChecksFailed); err != nil {
		return out, err
	}
	if usageTables == 0 {
		return out, nil
	}
	rows, err = s.db.Query("SELECT data FROM usage_reports ORDER BY recorded_at,id")
	if err != nil {
		return out, err
	}
	models := map[string]*ui.ModelStatistics{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return out, err
		}
		var report UsageReport
		if err = json.Unmarshal([]byte(raw), &report); err != nil {
			rows.Close()
			return out, err
		}
		if len(report.Models) == 0 {
			continue
		}
		out.UsageReports++
		if len(report.Time) >= 10 {
			if index, ok := dates[report.Time[:10]]; ok {
				out.Days[index].Reports++
			}
		}
		addUSD(&out.ReportedUSD, report.ReportedUSD)
		if report.PricingDate > out.PricingDate {
			out.PricingDate = report.PricingDate
			if len(out.PricingDate) > 10 {
				out.PricingDate = out.PricingDate[:10]
			}
		}
		allPriced := true
		modelPriced := map[string]bool{}
		for _, u := range report.Models {
			name := u.Model
			if name == "" {
				name = "unknown model"
			}
			model := models[name]
			if model == nil {
				model = &ui.ModelStatistics{Model: name}
				models[name] = model
			}
			model.Input += u.Input
			model.Output += u.Output
			model.CacheRead += u.CacheRead
			model.CacheWrite += u.CacheWrite
			if _, seen := modelPriced[name]; !seen {
				modelPriced[name] = true
			}
			out.Input += u.Input
			out.Output += u.Output
			out.CacheRead += u.CacheRead
			out.Tokens += u.Input + u.Output
			addUSD(&model.EstimatedUSD, u.EstimatedUSD)
			addUSD(&model.ReportedUSD, u.ReportedUSD)
			addUSD(&out.EstimatedUSD, u.EstimatedUSD)
			if u.EstimatedUSD == nil {
				allPriced = false
				modelPriced[name] = false
			}
			if len(report.Time) >= 10 {
				if index, ok := dates[report.Time[:10]]; ok {
					out.Days[index].Tokens += u.Input + u.Output
					addUSD(&out.Days[index].EstimatedUSD, u.EstimatedUSD)
				}
			}
		}
		for name, priced := range modelPriced {
			models[name].Reports++
			if priced {
				models[name].Priced++
			}
		}
		if allPriced {
			out.PricedReports++
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, model := range models {
		out.Models = append(out.Models, *model)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		a, b := out.Models[i], out.Models[j]
		if a.Input+a.Output != b.Input+b.Output {
			return a.Input+a.Output > b.Input+b.Output
		}
		return strings.Compare(a.Model, b.Model) < 0
	})
	return out, nil
}
