package commands

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newInspectCmd() *cobra.Command {
	var (
		agent   string
		evType  string
		session string
		since   string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Print recent event payloads (default 10)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(agent, evType, session, since, limit)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "Filter by agent slug")
	cmd.Flags().StringVar(&evType, "type", "", "Filter by event_type")
	cmd.Flags().StringVar(&session, "session", "", "Filter by session_id")
	cmd.Flags().StringVar(&since, "since", "", "Only events with ts >= ISO timestamp")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum number of events to print")
	return cmd
}

func runInspect(agent, evType, session, since string, limit int) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}

	var where []string
	var params []any
	if agent != "" {
		where = append(where, "agent_slug = ?")
		params = append(params, agent)
	}
	if evType != "" {
		where = append(where, "event_type = ?")
		params = append(params, evType)
	}
	if session != "" {
		where = append(where, "session_id = ?")
		params = append(params, session)
	}
	if since != "" {
		where = append(where, "ts >= ?")
		params = append(params, since)
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT id, ts, event_type, agent_slug, session_id, cwd, file_path, rel_file_path, payload
		FROM events
		%s
		ORDER BY ts DESC
		LIMIT %d`, whereClause, limit)

	rows, err := db.Query(q, params...)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id, ts, eventType, agentSlug string
		var sessionID, cwd, filePath, relFilePath, payload sql.NullString
		if err := rows.Scan(&id, &ts, &eventType, &agentSlug, &sessionID, &cwd, &filePath, &relFilePath, &payload); err != nil {
			return err
		}
		block := map[string]any{
			"ts":            ts,
			"agent":         agentSlug,
			"event_type":    eventType,
			"session_id":    nullToAny(sessionID),
			"cwd":           nullToAny(cwd),
			"file_path":     nullToAny(filePath),
			"rel_file_path": nullToAny(relFilePath),
		}
		if payload.Valid {
			var parsed any
			if err := json.Unmarshal([]byte(payload.String), &parsed); err == nil {
				block["payload"] = parsed
			} else {
				block["payload"] = payload.String
			}
		} else {
			block["payload"] = nil
		}
		data, _ := json.MarshalIndent(block, "", "  ")
		fmt.Println(string(data))
		fmt.Println("---")
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		fmt.Println("(no matching events)")
		return nil
	}
	plural := "s"
	if count == 1 {
		plural = ""
	}
	fmt.Printf("(%d event%s)\n", count, plural)
	return nil
}

func nullToAny(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}
