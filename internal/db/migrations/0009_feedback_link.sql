-- T11: User feedback association — index the lookup path for the
-- 30-minute associated_event_id → issue window (DESIGN.md §1, ticket #45).
CREATE INDEX IF NOT EXISTS feedbacks_associated_event_idx
    ON feedbacks (project_id, associated_event_id)
    WHERE associated_event_id IS NOT NULL;
