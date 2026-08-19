-- 0009_entity_mentions: the bridge between the graph and memory rows.
--
-- edges join entities to entities and nothing else, so a graph walk had no
-- way back to a fact or an experience. Both carry source_id, so an entity
-- knowing which sources mentioned it is enough to get there.
--
-- An entity is upserted once and mentioned by many sources, so a source_id
-- column on entities would be wrong; this is the honest shape.

-- +goose Up
CREATE TABLE entity_mentions (
    entity_id  UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    source_id  UUID NOT NULL REFERENCES sources(id)  ON DELETE CASCADE,
    PRIMARY KEY (entity_id, source_id)
);
CREATE INDEX entity_mentions_source ON entity_mentions (source_id);

-- +goose Down
DROP TABLE IF EXISTS entity_mentions;
