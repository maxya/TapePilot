-- Existing publications retain unknown counts until a new restore is run.
ALTER TABLE restore_publications ADD COLUMN selected_members INTEGER CHECK(selected_members >= 0);
ALTER TABLE restore_publications ADD COLUMN expanded_members INTEGER CHECK(expanded_members >= 0 AND expanded_members <= selected_members);
