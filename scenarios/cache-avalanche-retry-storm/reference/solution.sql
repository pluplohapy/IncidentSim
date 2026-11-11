-- Fix the connection limit the fault squeezed: the role must be able to hold
-- the pool the application needs again. A raised limit only hides the
-- avalanche under higher errors, while the restored limit lets the bounded
-- retry loop work.
ALTER ROLE postgres CONNECTION LIMIT -1;
