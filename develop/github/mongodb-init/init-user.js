// Create Temporal test user
const admin = db.getSiblingDB("admin");
const user = "temporal";
const pwd = "temporal";
try {
  admin.createUser({ user, pwd, roles: [{ role: "root", db: "admin" }] });
  print("Created user temporal");
} catch (e) {
  if (e.codeName === "DuplicateKey") {
    print("User temporal already exists");
  } else {
    throw e;
  }
}
