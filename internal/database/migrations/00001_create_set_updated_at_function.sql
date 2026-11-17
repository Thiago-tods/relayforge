-- Purpose: provide the shared trigger function that keeps "updated_at" columns
-- current. It is the first reusable schema object and also proves that the
-- migration pipeline works end to end. No business tables exist yet.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION relayforge_set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- Safe to reverse while no trigger references the function. Later migrations
-- that create triggers must drop them before this migration is reverted.
DROP FUNCTION relayforge_set_updated_at();
