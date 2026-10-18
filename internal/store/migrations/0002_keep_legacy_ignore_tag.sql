-- The project was renamed from Plex Automations to Topiarr, and the default
-- ignore tag with it. An install that already has an account was running
-- with the old default, and its Radarr/Sonarr items carry that tag, so pin
-- the old name explicitly unless one was chosen. Fresh installs run this
-- before any account exists, so they get the new default.
INSERT INTO settings (key, value, updated_at)
SELECT 'library.ignore_tag', 'plex-automations-ignore', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
 WHERE EXISTS (SELECT 1 FROM users)
   AND NOT EXISTS (SELECT 1 FROM settings WHERE key = 'library.ignore_tag');
