-- Runs once, on the first start of an empty postgres volume: the backend's
-- DB tests (TEST_DATABASE_URL) use their own database so they can truncate
-- freely without wiping the dev data in "cloud".
CREATE DATABASE cloud_test OWNER cloud;
