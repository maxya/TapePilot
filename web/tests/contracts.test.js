import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import Ajv from "ajv";
import formats from "ajv-formats";
/** @param {string} name */
const json = (name) =>
  JSON.parse(
    readFileSync(
      new URL("../../schemas/" + name + ".json", import.meta.url),
      "utf8",
    ),
  );
const validator = new Ajv();
formats(validator);
for (const name of ["volume", "commit", "descriptor", "config"]) {
  test(
    name + " accepts the synthetic fixture and rejects unknown fields",
    () => {
      const validate = validator.compile(json(name));
      const fixture = json("fixtures/" + name);
      assert.equal(
        validate(fixture),
        true,
        validator.errorsText(validate.errors),
      );
      assert.equal(validate({ ...fixture, extra: true }), false);
      if (name === "commit")
        assert.equal(validate({ ...fixture, sequence: 2 }), false);
      if (name === "volume")
        assert.equal(
          validate({
            ...fixture,
            tape_profile: { ...fixture.tape_profile, record_bytes: 512 },
          }),
          false,
        );
    },
  );
}
