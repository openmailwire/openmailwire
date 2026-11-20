-- The dashboard overview (v0.6.1): count the messages held from agent keys
-- without reading every inbound message. Only held rows are indexed, so the
-- index stays small: messages leave it when released.
CREATE INDEX inbound_messages_held ON inbound_messages (project_id, received_at) WHERE held;
