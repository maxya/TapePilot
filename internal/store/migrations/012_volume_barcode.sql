ALTER TABLE volumes ADD COLUMN barcode TEXT NOT NULL DEFAULT '' CHECK(length(barcode)<=128);
