package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type RelayMemory struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	Source    string    `json:"source"`
	RequestID string    `json:"request_id,omitempty"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

const relayMemoryColumns = `id,body,source,coalesce(request_id,''),version,updated_at`

func scanRelayMemory(row pgx.Row) (RelayMemory, error) {
	var item RelayMemory
	err := row.Scan(&item.ID, &item.Body, &item.Source, &item.RequestID, &item.Version, &item.UpdatedAt)
	return item, err
}

func (s *Store) RelayMemories(ctx context.Context, org, user, search string, offset int) ([]RelayMemory, error) {
	rows, err := s.Pool.Query(ctx, `select `+relayMemoryColumns+` from billing.relay_memories
		where org_id=$1 and user_id=$2 and ($3='' or strpos(lower(body),lower($3))>0)
		order by updated_at desc,id desc limit 51 offset $4`, org, user, search, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RelayMemory, error) { return scanRelayMemory(row) })
}

func (s *Store) SaveRelayMemory(ctx context.Context, org, user string, item RelayMemory) (RelayMemory, error) {
	var row pgx.Row
	if item.ID == "" {
		row = s.Pool.QueryRow(ctx, `insert into billing.relay_memories (org_id,user_id,body,source)
			values ($1,$2,$3,'manual') returning `+relayMemoryColumns, org, user, item.Body)
	} else {
		row = s.Pool.QueryRow(ctx, `update billing.relay_memories set body=$4,version=version+1,updated_at=clock_timestamp()
			where org_id=$1 and user_id=$2 and id=$3 and version=$5 returning `+relayMemoryColumns,
			org, user, item.ID, item.Body, item.Version)
	}
	return scanRelayMemory(row)
}

func (s *Store) DeleteRelayMemories(ctx context.Context, org, user string, ids []string, all bool) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// Serialize with writes and invalidate every work item using an earlier snapshot.
	if err = lockRelayPreference(ctx, tx, org, user); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `update billing.relay_preferences set revision=revision+1 where org_id=$1 and principal=$2`, org, user); err != nil {
		return 0, err
	}
	deleted, err := tx.Exec(ctx, `delete from billing.relay_memories where org_id=$1 and user_id=$2
		and ($3 or id::text=any($4::text[]))`, org, user, all, ids)
	if err != nil {
		return 0, err
	}
	if all {
		if _, err = tx.Exec(ctx, `delete from billing.relay_memory_events where org_id=$1 and user_id=$2`, org, user); err != nil {
			return 0, err
		}
	}
	return deleted.RowsAffected(), tx.Commit(ctx)
}

type RelayMemoryEvent struct {
	ID        int64     `json:"id"`
	RequestID string    `json:"request_id"`
	Phase     string    `json:"phase"`
	Status    string    `json:"status"`
	MemoryIDs []string  `json:"memory_ids"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) RelayMemoryEvents(ctx context.Context, org, user string, after int64) ([]RelayMemoryEvent, error) {
	rows, err := s.Pool.Query(ctx, `select id,request_id,phase,status,array(select m.id::text from billing.relay_memories m
		where m.org_id=e.org_id and m.user_id=e.user_id and m.id::text=any(e.memory_ids)),created_at
		from billing.relay_memory_events e where org_id=$1 and user_id=$2 and id>$3 and created_at>now()-interval '1 day'
		order by case when $3=0 then -id else id end limit 100`, org, user, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RelayMemoryEvent, error) {
		var e RelayMemoryEvent
		err := row.Scan(&e.ID, &e.RequestID, &e.Phase, &e.Status, &e.MemoryIDs, &e.CreatedAt)
		return e, err
	})
}

type RelayMemoryCall struct {
	OrgID        string `json:"org_id"`
	UserID       string `json:"user_id"`
	RequestID    string `json:"request_id"`
	Action       string `json:"action"`
	Text         string `json:"text"`
	UserRevision int64  `json:"user_revision"`
	OrgRevision  int64  `json:"org_revision"`
}

func (s *Store) RunRelayMemory(ctx context.Context, call RelayMemoryCall) ([]RelayMemory, error) {
	org, user := call.OrgID, call.UserID
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// The two preference locks fence old async jobs across setting changes and clears.
	if _, err = tx.Exec(ctx, `insert into billing.relay_preferences (org_id,principal)
		values ($1,'') on conflict do nothing`, org); err != nil {
		return nil, err
	}
	var orgRevision int64
	if err = tx.QueryRow(ctx, `select revision from billing.relay_preferences
		where org_id=$1 and principal='' for share`, org).Scan(&orgRevision); err != nil {
		return nil, err
	}
	if err = lockRelayPreference(ctx, tx, org, user); err != nil {
		return nil, err
	}
	prefs, err := readRelayPreferences(ctx, tx, org, user)
	if err != nil {
		return nil, err
	}
	if prefs.Effective.MemoryEnabled == nil || !*prefs.Effective.MemoryEnabled ||
		prefs.UserRevision != call.UserRevision || orgRevision != call.OrgRevision {
		return nil, ErrNotFound
	}
	phase, status := call.Action, "ok"
	var result []RelayMemory
	switch call.Action {
	case "search":
		rows, queryErr := tx.Query(ctx, `select `+relayMemoryColumns+` from billing.relay_memories
			where org_id=$1 and user_id=$2 and (
			  exists (select 1 from regexp_split_to_table(lower($3), '[^[:alnum:]]+') as q(word)
			    where length(word)>=3 and word not in ('the','and','what','about','with','you','that','this','have')
			    and strpos(lower(body),word)>0)
			  or exists (select 1 from generate_series(1,char_length($3)-1) as q(i)
			    where substring($3 from i for 2) ~ '[一-龥]{2}' and strpos(body,substring($3 from i for 2))>0))
			order by updated_at desc,id desc limit 4`, org, user, call.Text)
		if queryErr != nil {
			return nil, queryErr
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RelayMemory, error) { return scanRelayMemory(row) })
		phase = "match"
		if len(result) == 0 {
			status = "miss"
		}
	case "write":
		item, saveErr := scanRelayMemory(tx.QueryRow(ctx, `insert into billing.relay_memories
			(org_id,user_id,body,source,request_id) values ($1,$2,$3,'model',$4)
			on conflict (org_id,user_id,request_id) do nothing returning `+relayMemoryColumns,
			org, user, call.Text, call.RequestID))
		if errors.Is(saveErr, pgx.ErrNoRows) {
			return nil, nil
		}
		if saveErr != nil {
			return nil, saveErr
		}
		result = []RelayMemory{item}
	case "start":
		phase, status = "search", "running"
	case "search_error":
		phase, status = "match", "error"
	case "write_error":
		phase, status = "write", "error"
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(result))
	for _, item := range result {
		ids = append(ids, item.ID)
	}
	if _, err = tx.Exec(ctx, `insert into billing.relay_memory_events (org_id,user_id,request_id,phase,status,memory_ids)
		values ($1,$2,$3,$4,$5,$6)`, org, user, call.RequestID, phase, status, ids); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `delete from billing.relay_memory_events where org_id=$1 and user_id=$2 and
		(created_at < now()-interval '1 day' or id <= coalesce((select id from billing.relay_memory_events
		where org_id=$1 and user_id=$2 order by id desc offset 499 limit 1),0))`, org, user); err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}
