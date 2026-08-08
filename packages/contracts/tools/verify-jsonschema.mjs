#!/usr/bin/env node

// Execute public-edge fixtures against the generated Draft 2020-12 schemas.
// Shape-only fixture metadata cannot prove nested required fields, bounds, or
// additional-property rejection, so this gate compiles the actual artifacts.

import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

const packageDir = join(dirname(fileURLToPath(import.meta.url)), "..");
const schemaDir = join(packageDir, "gen", "jsonschema");

const id = (namespace, value) => ({ namespace, value });
const digest = (hex = "a".repeat(64)) => ({ algorithm: "sha256", hex });
const principal = () => ({
  principalId: id("principal", "user-1"),
  tenantId: id("tenant", "tenant-1"),
});
const causal = () => ({
  correlationId: id("operation", "correlation-1"),
  causationId: id("event", "causation-1"),
  traceId: id("trace", "trace-1"),
  sequence: 1,
  fence: 1,
  watermark: 1,
});
const evidenceRef = () => ({
  evidenceId: id("evidence", "evidence-1"),
  sourceRevisionId: id("source-revision", "revision-1"),
});
const artifact = (value) => ({
  artifactId: id("artifact", value),
  contentDigest: digest(),
  tenantId: id("tenant", "tenant-1"),
});
const receipt = (value) => ({
  receiptId: id("receipt", value),
  status: "RECEIPT_STATUS_COMPLETED",
  reasonCode: "ok",
  operationId: id("operation", "operation-1"),
  causal: causal(),
  recordedAt: "2026-07-19T00:00:00Z",
  evidence: [],
  configurationDigest: digest(),
});
const approval = () => ({
  approvalId: id("approval", "approval-1"),
  approver: principal(),
  scopeDigest: digest(),
  expiresAt: "2026-07-20T00:00:00Z",
  receipt: receipt("approval-receipt-1"),
});

const eventEnvelope = {
  eventId: id("event", "event-1"),
  eventType: "evidence.admitted.v1",
  actor: principal(),
  occurredAt: "2026-07-19T00:00:00Z",
  idempotencyKey: "event-idempotency-1",
  causal: causal(),
  payloadDigest: digest(),
};

const positives = {
  EventEnvelope: eventEnvelope,
  CommandEnvelope: {
    commandId: id("command", "command-1"),
    commandType: "evidence.admit.v1",
    actor: principal(),
    submittedAt: "2026-07-19T00:00:00Z",
    idempotencyKey: "command-idempotency-1",
    causal: causal(),
    payloadDigest: digest(),
  },
  BrainQuery: {
    queryId: id("query", "query-1"),
    query: "What supports the requested change?",
    requestedPrincipal: principal(),
    scopes: [{ brainId: id("brain", "company-1"), scopeKind: "company" }],
    includeOwnPriorSessions: false,
    includeSharedSessions: false,
    repositoryGitOid: "b".repeat(40),
    tokenBudget: 4096,
    freshness: "FRESHNESS_REQUIREMENT_COMPLETE_GENERATION",
  },
  GroundedAnswer: {
    queryId: id("query", "query-1"),
    status: "ANSWER_STATUS_ABSTAINED",
    prose: "",
    claims: [],
    validAt: "2026-07-19T00:00:00Z",
    knownAt: "2026-07-19T00:00:00Z",
    consistencyWatermark: 1,
    aclEpoch: 1,
    coveragePerMille: 0,
    degradedReasons: ["supporting_span_absent"],
    routingReceipt: receipt("routing-receipt-1"),
    tokenUsage: 64,
    factualConsistency: {
      status: "FACTUAL_CONSISTENCY_STATUS_ABSTAINED",
      reason: "FACTUAL_CONSISTENCY_REASON_ANSWER_ABSTAINED",
      scorePerMille: 0,
      evaluatedClaimCount: 0,
      totalClaimCount: 0,
    },
  },
  ChangeIntent: {
    intentId: id("change-intent", "intent-1"),
    requestedBy: principal(),
    repositoryGitOid: "c".repeat(40),
    scopeDigest: digest(),
    supportingEvidence: [evidenceRef()],
    approval: approval(),
  },
  ChangeSet: {
    changeSetId: id("change-set", "change-set-1"),
    baseGitOid: "c".repeat(40),
    patchArtifact: artifact("patch-1"),
    changeSetDigest: digest(),
    validationReceipts: [receipt("validation-receipt-1")],
    rollbackArtifact: artifact("rollback-1"),
  },
  ChannelDelivery: {
    deliveryId: "provider-delivery-1",
    gatewayId: id("channel-gateway", "gateway-1"),
    payloadArtifact: artifact("channel-payload-1"),
    receivedAt: "2026-07-19T00:00:00Z",
    replayReceipt: receipt("replay-receipt-1"),
  },
  CapabilityGrant: {
    grantId: id("grant", "grant-1"),
    initiator: principal(),
    actions: ["artifact.read"],
    resources: [id("artifact", "artifact-1")],
    repositoryGitOid: "",
    nonce: "grant-nonce-1",
    revocationEpoch: 1,
    expiresAt: "2026-07-20T00:00:00Z",
    policyDigest: digest(),
    commandFence: 7,
  },
};

const clone = (value) => structuredClone(value);
const without = (schemaName, path) => {
  const value = clone(positives[schemaName]);
  const parent = path.slice(0, -1).reduce((cursor, part) => cursor[part], value);
  delete parent[path.at(-1)];
  return value;
};
const withValue = (schemaName, path, replacement) => {
  const value = clone(positives[schemaName]);
  const parent = path.slice(0, -1).reduce((cursor, part) => cursor[part], value);
  parent[path.at(-1)] = replacement;
  return value;
};
const withUnknownRootField = (schemaName) => ({
  ...clone(positives[schemaName]),
  ambientAuthority: true,
});

// Every public schema must reject the same boundary-risk categories. Each
// vector exercises its generated schema directly; the coverage matrix below
// also prevents a future schema from receiving positive-only coverage.
const rejectionCases = [
  {
    schema: "EventEnvelope",
    category: "missing",
    name: "event ID is required",
    value: without("EventEnvelope", ["eventId"]),
  },
  {
    schema: "EventEnvelope",
    category: "wrong-type",
    name: "event type must be a string",
    value: withValue("EventEnvelope", ["eventType"], 42),
  },
  {
    schema: "EventEnvelope",
    category: "bounds",
    name: "idempotency key is capped at 512 characters",
    value: withValue("EventEnvelope", ["idempotencyKey"], "x".repeat(513)),
  },
  {
    schema: "EventEnvelope",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("EventEnvelope"),
  },
  {
    schema: "EventEnvelope",
    category: "nested",
    name: "nested event ID cannot be empty",
    value: withValue("EventEnvelope", ["eventId", "value"], ""),
  },
  {
    schema: "CommandEnvelope",
    category: "missing",
    name: "command ID is required",
    value: without("CommandEnvelope", ["commandId"]),
  },
  {
    schema: "CommandEnvelope",
    category: "wrong-type",
    name: "submission timestamp must be a string",
    value: withValue("CommandEnvelope", ["submittedAt"], 42),
  },
  {
    schema: "CommandEnvelope",
    category: "bounds",
    name: "idempotency key is capped at 512 characters",
    value: withValue("CommandEnvelope", ["idempotencyKey"], "x".repeat(513)),
  },
  {
    schema: "CommandEnvelope",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("CommandEnvelope"),
  },
  {
    schema: "CommandEnvelope",
    category: "nested",
    name: "nested actor tenant ID cannot be empty",
    value: withValue("CommandEnvelope", ["actor", "tenantId", "value"], ""),
  },
  {
    schema: "BrainQuery",
    category: "missing",
    name: "freshness requirement is required",
    value: without("BrainQuery", ["freshness"]),
  },
  {
    schema: "BrainQuery",
    category: "wrong-type",
    name: "token budget must be an integer",
    value: withValue("BrainQuery", ["tokenBudget"], "4096"),
  },
  {
    schema: "BrainQuery",
    category: "bounds",
    name: "token budget must be greater than zero",
    value: withValue("BrainQuery", ["tokenBudget"], 0),
  },
  {
    schema: "BrainQuery",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("BrainQuery"),
  },
  {
    schema: "BrainQuery",
    category: "nested",
    name: "nested requested principal ID cannot be empty",
    value: withValue("BrainQuery", ["requestedPrincipal", "principalId", "value"], ""),
  },
  {
    schema: "GroundedAnswer",
    category: "missing",
    name: "routing receipt is required",
    value: without("GroundedAnswer", ["routingReceipt"]),
  },
  {
    schema: "GroundedAnswer",
    category: "wrong-type",
    name: "coverage must be an integer",
    value: withValue("GroundedAnswer", ["coveragePerMille"], "0"),
  },
  {
    schema: "GroundedAnswer",
    category: "bounds",
    name: "coverage cannot exceed one thousand per mille",
    value: withValue("GroundedAnswer", ["coveragePerMille"], 1001),
  },
  {
    schema: "GroundedAnswer",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("GroundedAnswer"),
  },
  {
    schema: "GroundedAnswer",
    category: "nested",
    name: "nested routing digest must be lower-case hexadecimal",
    value: withValue("GroundedAnswer", ["routingReceipt", "configurationDigest", "hex"], "not-hex"),
  },
  {
    schema: "ChangeIntent",
    category: "missing",
    name: "approval is required",
    value: without("ChangeIntent", ["approval"]),
  },
  {
    schema: "ChangeIntent",
    category: "wrong-type",
    name: "repository Git OID must be a string",
    value: withValue("ChangeIntent", ["repositoryGitOid"], 42),
  },
  {
    schema: "ChangeIntent",
    category: "bounds",
    name: "at least one supporting evidence reference is required",
    value: withValue("ChangeIntent", ["supportingEvidence"], []),
  },
  {
    schema: "ChangeIntent",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("ChangeIntent"),
  },
  {
    schema: "ChangeIntent",
    category: "nested",
    name: "nested approver tenant ID cannot be empty",
    value: withValue("ChangeIntent", ["approval", "approver", "tenantId", "value"], ""),
  },
  {
    schema: "ChangeSet",
    category: "missing",
    name: "rollback artifact is required",
    value: without("ChangeSet", ["rollbackArtifact"]),
  },
  {
    schema: "ChangeSet",
    category: "wrong-type",
    name: "base Git OID must be a string",
    value: withValue("ChangeSet", ["baseGitOid"], 42),
  },
  {
    schema: "ChangeSet",
    category: "bounds",
    name: "at least one validation receipt is required",
    value: withValue("ChangeSet", ["validationReceipts"], []),
  },
  {
    schema: "ChangeSet",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("ChangeSet"),
  },
  {
    schema: "ChangeSet",
    category: "nested",
    name: "nested patch artifact digest must be lower-case hexadecimal",
    value: withValue("ChangeSet", ["patchArtifact", "contentDigest", "hex"], "not-hex"),
  },
  {
    schema: "ChannelDelivery",
    category: "missing",
    name: "replay receipt is required",
    value: without("ChannelDelivery", ["replayReceipt"]),
  },
  {
    schema: "ChannelDelivery",
    category: "wrong-type",
    name: "received timestamp must be a string",
    value: withValue("ChannelDelivery", ["receivedAt"], 42),
  },
  {
    schema: "ChannelDelivery",
    category: "bounds",
    name: "provider delivery ID is capped at 512 characters",
    value: withValue("ChannelDelivery", ["deliveryId"], "x".repeat(513)),
  },
  {
    schema: "ChannelDelivery",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("ChannelDelivery"),
  },
  {
    schema: "ChannelDelivery",
    category: "nested",
    name: "nested payload tenant ID cannot be empty",
    value: withValue("ChannelDelivery", ["payloadArtifact", "tenantId", "value"], ""),
  },
  {
    schema: "CapabilityGrant",
    category: "missing",
    name: "grant ID is required",
    value: without("CapabilityGrant", ["grantId"]),
  },
  {
    schema: "CapabilityGrant",
    category: "wrong-type",
    name: "command fence must be an integer",
    value: withValue("CapabilityGrant", ["commandFence"], "7"),
  },
  {
    schema: "CapabilityGrant",
    category: "bounds",
    name: "wildcard actions are invalid schema shape",
    value: withValue("CapabilityGrant", ["actions"], ["*"]),
  },
  {
    schema: "CapabilityGrant",
    category: "unknown",
    name: "ambient authority is not a declared field",
    value: withUnknownRootField("CapabilityGrant"),
  },
  {
    schema: "CapabilityGrant",
    category: "nested",
    name: "nested initiator principal cannot be empty",
    value: withValue("CapabilityGrant", ["initiator", "principalId", "value"], ""),
  },
];

const ajv = new Ajv2020({ allErrors: true, strict: true });
addFormats(ajv);

const validators = new Map();
for (const [name, value] of Object.entries(positives)) {
  const path = join(schemaDir, `ouroboros.contracts.v1.${name}.jsonschema.strict.bundle.json`);
  const schema = JSON.parse(await readFile(path, "utf8"));
  const validate = ajv.compile(schema);
  validators.set(name, validate);
  if (!validate(value)) {
    throw new Error(`positive ${name} fixture failed: ${ajv.errorsText(validate.errors)}`);
  }
}

const requiredCategories = new Set(["missing", "wrong-type", "bounds", "unknown", "nested"]);
for (const schemaName of validators.keys()) {
  const covered = new Set(
    rejectionCases.filter(({ schema }) => schema === schemaName).map(({ category }) => category),
  );
  const missing = [...requiredCategories].filter((category) => !covered.has(category));
  if (missing.length > 0) {
    throw new Error(`negative ${schemaName} coverage is missing: ${missing.join(", ")}`);
  }
}

for (const { schema, category, name, value } of rejectionCases) {
  const validate = validators.get(schema);
  if (!validate) {
    throw new Error(`negative fixture references unknown public schema: ${schema}`);
  }
  if (validate(value)) {
    throw new Error(`negative ${schema} ${category} fixture unexpectedly passed: ${name}`);
  }
}

console.log(`validated ${validators.size} positive schemas and ${rejectionCases.length} rejection vectors`);
