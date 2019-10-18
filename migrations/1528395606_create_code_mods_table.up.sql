BEGIN;

CREATE TABLE code_mods (
  id bigserial PRIMARY KEY,
  code_mod_spec text NOT NULL CHECK (code_mod_spec != ''),
  arguments jsonb NOT NULL DEFAULT '{}'
    CHECK (jsonb_typeof(arguments) = 'object'),
  created_at timestamp with time zone NOT NULL DEFAULT now(),
  updated_at timestamp with time zone NOT NULL DEFAULT now()
);

ALTER TABLE campaigns
ADD COLUMN code_mod_id integer REFERENCES code_mods(id)
DEFERRABLE INITIALLY IMMEDIATE;

COMMIT;

