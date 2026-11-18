-- Whether this database was created by a development start (email login, demo data) or a
-- deployment start (no email login, no demo data). It is written once; a later start of the other
-- kind is refused so demo accounts never appear in a deployment and the reverse.
CREATE TABLE v2_instance (
	singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
	profile    text NOT NULL CHECK (profile IN ('development', 'deployment')),
	created_at timestamptz NOT NULL DEFAULT now()
);
