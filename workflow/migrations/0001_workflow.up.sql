-- The worklist (docs/design.md, the data): the task projections of the chosen publishers, by version; their candidates
-- now and ever; the publishers' last signs of life; the refused projections.
CREATE TABLE task (
    component       text NOT NULL,
    id              uuid NOT NULL,
    task_type       text NOT NULL,
    state           text NOT NULL,
    legal_entity_id uuid,
    due_at          timestamptz,
    decided_by      text,
    data            jsonb NOT NULL,
    version         bigint NOT NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (component, id)
);
CREATE INDEX task_ended ON task (received_at DESC, component DESC, id DESC) WHERE state IN ('decided', 'superseded');
CREATE INDEX task_decided_by ON task (decided_by) WHERE decided_by IS NOT NULL;

CREATE TABLE task_candidate (
    component text NOT NULL,
    task_id   uuid NOT NULL,
    principal text NOT NULL,
    current   boolean NOT NULL,
    PRIMARY KEY (component, task_id, principal),
    FOREIGN KEY (component, task_id) REFERENCES task (component, id)
);
CREATE INDEX task_candidate_mine ON task_candidate (principal) WHERE current;
CREATE INDEX task_candidate_ever ON task_candidate (principal);

CREATE TABLE publisher (
    component       text PRIMARY KEY,
    last_heartbeat  timestamptz,
    last_projection timestamptz
);

CREATE TABLE rejected_projection (
    id        uuid PRIMARY KEY,
    component text NOT NULL,
    task_type text NOT NULL,
    task_id   text NOT NULL,
    reason    text NOT NULL CHECK (reason IN ('task_type_not_configured', 'not_the_chosen_publisher', 'malformed')),
    at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rejected_projection_at ON rejected_projection (at DESC, id DESC);
