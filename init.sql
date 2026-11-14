-- Runs before anything else, in every molt session: settings, secrets, variables
-- and macros. Override variables per environment with --var, for example
-- `molt run --var DATA_DIR=/mnt/volume/feeds` on the server.
SET VARIABLE DATA_DIR = 'example_flow';
