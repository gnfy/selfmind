package control

import "context"

// QueuedHostExecutionWaits derives writer intent only from exact typed pending
// dispatches. A claimed child's bounded policy checks retain its place until it
// enters execution or explicitly yields for another resource/human. No row here
// grants permission to execute.
func (s *Store) QueuedHostExecutionWaits(ctx context.Context, tenantID, personID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT q.reply_to_run_id FROM task_queue q
 JOIN loop_checkpoints c ON c.tenant_id=q.tenant_id AND c.person_id=q.person_id AND c.run_id=q.reply_to_run_id
 JOIN runs parent ON parent.tenant_id=q.tenant_id AND parent.person_id=q.person_id AND parent.id=q.reply_to_run_id
 LEFT JOIN runs child ON child.tenant_id=q.tenant_id AND child.person_id=q.person_id AND child.id=q.run_id
 WHERE q.tenant_id=? AND q.person_id=? AND q.idempotency_key='execution-lane:'||parent.id
 AND c.outcome='await_execution' AND c.wait_kind='execution_host_lane' AND c.wait_ref=parent.id AND c.wait_tool_call_id<>''
 AND parent.status='waiting_external'
 AND (q.status='queued' OR (q.status='started' AND (COALESCE(q.run_id,'')='' OR child.status='running')))
 ORDER BY q.created_at,q.rowid`, normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
