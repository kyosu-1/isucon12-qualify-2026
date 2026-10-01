CREATE TABLE competition (
  id VARCHAR(255) NOT NULL PRIMARY KEY,
  tenant_id BIGINT NOT NULL,
  title TEXT NOT NULL,
  finished_at BIGINT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE TABLE player (
  id VARCHAR(255) NOT NULL PRIMARY KEY,
  tenant_id BIGINT NOT NULL,
  display_name TEXT NOT NULL,
  is_disqualified BOOLEAN NOT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE TABLE player_score (
  competition_id VARCHAR(255) NOT NULL,
  player_id VARCHAR(255) NOT NULL,
  score BIGINT NOT NULL,
  row_num BIGINT NOT NULL
);
CREATE TABLE visit_history (
  competition_id VARCHAR(255) NOT NULL,
  player_id VARCHAR(255) NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE TABLE billing_report (
  competition_id VARCHAR(255) NOT NULL PRIMARY KEY,
  player_count BIGINT NOT NULL,
  visitor_count BIGINT NOT NULL
);
CREATE INDEX player_score_comp ON player_score (competition_id);
CREATE INDEX visit_history_comp ON visit_history (competition_id);
