-- a bit dirty because it only works if there are no licenses yet, but since this feature isn't released yet, should be fine
ALTER TABLE ArtifactLicense
  ADD COLUMN organization_id UUID NOT NULL REFERENCES Organization (id),
  ADD CONSTRAINT ArtifactLicense_name_unique UNIQUE (organization_id, name);

CREATE INDEX fk_ArtifactLicense_organization_id ON ArtifactLicense (organization_id);
