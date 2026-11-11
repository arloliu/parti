# Recreating Durable Consumers to Change `MaxWaiting`

> `MaxWaiting` cannot be changed on an existing JetStream durable consumer.
> Changing it means stopping every consumer instance, deleting the durables,
> and letting the instances create them again with the new value.
> Most deployments never need to.

**Related Documentation:**
- [Consumers Guide](CONSUMERS.md) — consumer types, stream retention policy
- [Operations Guide](OPERATIONS.md) — deployment, scaling, rolling updates
- [Provision Guide](PROVISION.md) — `partictl`, the immutable-field contract

---

## Do you need this?

`MaxWaiting` caps the pull requests that can wait on one durable at the same time.
The server drops a pull request beyond the cap without a reply,
and the consumer notices only after a missed heartbeat.

In releases after v2.10.2 each running consumer instance keeps **one** pull request waiting,
whatever its `BatchSize`:

| Consumer    | Pull requests waiting on one durable       | `MaxWaiting` needed               |
|-------------|--------------------------------------------|-----------------------------------|
| `Dynamic`   | one per owner; two while a partition is handed over | 2 (the default)          |
| `Static`    | one per instance that reads the durable     | the number of those instances     |
| `Queue`     | one per instance; all instances share the durable | at least the instance count |
| `Broadcast` | one; every instance has its own durable     | 1                                 |

Earlier releases overlapped two or three pull requests per iterator when `BatchSize` was more than a few messages,
which is what made a small `MaxWaiting` stall delivery.
Upgrading parti removes that with an ordinary rolling update and no change to the durables.
**Upgrade first, and recreate durables only if the table above still calls for a larger value**,
typically a `Queue` scaled beyond its `MaxWaiting`.

Raising `MaxWaiting` for headroom alone is optional.
It costs a full stop of the consumers and has the consequences described below.

The same procedure applies to `AckPolicy`, which the server also refuses to update.

---

## Why a full stop is required

- The server rejects an update that changes `MaxWaiting`
  (`max waiting can not be updated`, error code 10012).
  The only way to a new value is to delete the durable and create it again.
- A durable holds the consumer's position: which messages were delivered, which were acknowledged,
  and how often each was delivered.
  Deleting it discards all of that.
- A running consumer that finds its durable gone may create it again with its own configuration.
  An instance still running with the old value would put the old value back.
- A rolling update runs old and new instances side by side on the same durables.
  Whichever value the durable has, the other half of the fleet cannot bind it.

So every instance that uses the durables must be stopped before they are deleted,
and no instance with the old value may start afterwards.

---

## Before you start

The recommended way to run the procedure is with `partictl`,
which checks most of what follows and refuses to delete when a check fails.
The same steps can be done by hand with the `nats` CLI;
see [Manual procedure](#manual-procedure-with-the-nats-cli).
Either way, knowing the consequences beforehand tells you whether the procedure is worth starting.

### 1. The stream's retention policy decides what a recreated durable receives

| `retention` | What the new durable receives | Proceed? |
|-------------|-------------------------------|----------|
| `workqueue` | Every message still in the stream: those never delivered, and those delivered but not acknowledged. Acknowledged messages are already gone. | Yes. |
| `limits`    | Every message retained on its subject, including all the ones already processed. | Only if the handlers can process the whole retained stream again. |
| `interest`  | Only what another consumer on the same subject still holds. Deleting the durable removes the messages nobody else holds, and messages published before it exists again are discarded unless another consumer holds them. Messages kept for another consumer are delivered to the new durable again. | No, unless the stream is empty and its publishers are stopped for the duration. |

These outcomes were measured on nats-server v2.14.3 (single node), with no other consumer on the subject.

### 2. Stop the publishers too, if you can

With the publishers stopped nothing new enters the stream while the consumers are down,
and the counts the command reports are exact.

If the publishers keep running, the backlog waits in the stream and the stream's limits apply to it:

- `max_msgs` / `max_bytes` with `discard: old`: when the backlog reaches the limit, the oldest messages are removed.
- `max_msgs` / `max_bytes` with `discard: new`: publishers get errors instead.

Either way, `max_age` still applies to the messages already in the stream:
a message older than that is removed, processed or not.
Keep the stop well below it.

### 3. Some messages are delivered a second time

A recreated durable starts with fresh delivery counts.
A message that was delivered before the stop but never acknowledged is delivered again.
That includes a message that had used up `MaxDeliver`, which the old durable would not have delivered again.
Its handler may already have run once.

After a clean shutdown there should be none.
The command counts them and refuses to delete while there are any, unless you accept them.

### 4. Find everything that uses the durables

- Every instance of every service that consumes from them has to be stopped, in every cluster and namespace.
- If the durables are precreated with `partictl consumers apply`, take the target out of precreation first.
  It creates them with `MaxWaiting` 2
  (see [the immutable-field contract](PROVISION.md#the-immutable-field-contract)).

---

## Procedure

Steps 2, 3 and 5 use `partictl`, the CLI in this repository.
This is the recommended way: the command makes the checks for you and deletes nothing while one of them fails.
For the same steps with the `nats` CLI, see [Manual procedure](#manual-procedure-with-the-nats-cli).

```sh
go build -o partictl ./cmd/partictl
export NATS_URL=nats://nats.example:4222
```

### Step 1 — Stop every instance and ship the new value, in one change

Deploy **zero replicas together with the new `MaxWaiting`** through your normal deployment channel.
If you are also upgrading parti, include the new version in the same change.

Shipping both at once means that from this point on,
any instance that starts already carries the new value, whatever restarts it.

With GitOps, make the change in Git.
A manual `kubectl scale --replicas=0` leaves the old value in the Deployment
and can be reverted by the sync controller in the middle of the procedure.

Wait until no pod is left:

```sh
kubectl get pods -l app=<your-app>     # must list nothing
```

**This step is the only thing that establishes the full stop, whatever the retention policy.**
The command in the next steps blocks on signs of activity,
but a consumer that is running and shows none of them looks the same as a stopped one:
one that has not pulled yet, is reconnecting, has its pulls suppressed, or is stuck in a handler.

### Step 2 — List the durables and check them

**A `Queue` or `Static` consumer** has one durable, with the consumer name you configured:

```sh
echo "my-queue-consumer" > durables.txt
```

**A `Dynamic` consumer** has one durable per partition.
Let the command find the candidates from the `ConsumerPrefix` and `SubjectTemplate`, and save their names:

```sh
partictl consumers delete -stream orders \
  -prefix orders-consumer -subject-template 'orders.{{.PartitionID}}' \
  -save-names durables.txt
```

Then review `durables.txt` and remove every line that is not a durable of this consumer.
The candidates are the durables named the way parti would name them for this prefix and template.
That cannot prove ownership:
a consumer with a longer prefix that reads deeper subjects,
such as `orders-consumer_eu` on `orders.eu.{{.PartitionID}}`, gets names of the same form.
For this reason the command never deletes a selection it discovered itself.

Now check the reviewed list.
Without `-apply` the command only reports:

```sh
partictl consumers delete -stream orders -names-file durables.txt
```

```text
Stream orders: retention WorkQueue, 3 message(s), max age 30m0s

DURABLE                               FILTER      MAX_WAITING  WAITING  ACK_PENDING  PENDING  EXHAUSTED
orders-consumer_0_cce5ea31ec987401    orders.0    2            0        0            3        0
orders-consumer_a_b_5487670cda1aff78  orders.a_b  2            0        0            0        0

2 durable(s) selected.
Warning: the stream removes messages older than 30m0s, processed or not: keep the consumers stopped for less.
Result: nothing deleted, no check blocked. Run again with -apply to delete.
```

It reads every durable twice, `-settle` apart (5s by default), and exits 2 when something blocks the delete:

| Blocked because | What to do |
|-----------------|------------|
| A durable has a pull request waiting, or delivered or acknowledged messages between the two readings. | A consumer instance is running. Find and stop it. Nothing overrides this. |
| The stream's retention is `limits` or `interest`. | See [the retention table](#1-the-streams-retention-policy-decides-what-a-recreated-durable-receives). Accept with `-allow-replay` or `-allow-loss`. |
| On a `workqueue` stream, messages were delivered without an acknowledgement (`ACK_PENDING`), or used up `MaxDeliver` (`EXHAUSTED`). | They will be delivered again. Accept with `-allow-redelivery`. |

"No check blocked" means only that: the checks cannot show that nothing is running.
Use an override only when Step 1 has established the full stop.
An instance that is stuck in a handler, for example, shows up as nothing but an unacknowledged message,
which `-allow-redelivery` and `-allow-replay` both accept.

A warning that the stream received messages means publishers are running.
The `EXHAUSTED` count is then an upper bound,
because a message published between two of the command's readings is counted as one.

Add `-json` to keep the report as a record of the old state:

```sh
partictl consumers delete -stream orders -names-file durables.txt -json > durables-before.json
```

### Step 3 — Delete the durables

This is the point of no return: the positions cannot be restored.

```sh
partictl consumers delete -stream orders -names-file durables.txt -apply
```

The command runs the same checks again and deletes only when none blocks.
It reads each durable once more just before deleting it,
and stops at the first one that changed since the checks.

If the run is interrupted, run it again; durables that are already gone are skipped.
The command works through the durables one at a time within `-timeout` (30s by default);
raise it for thousands of durables.

### Step 4 — Start the instances

Set the replica count back through the same deployment channel.
The instances create the durables again with the new value as they start.

### Step 5 — Verify

```sh
partictl consumers verify -stream orders -names-file durables.txt -max-waiting 16
```

The command exits 2 when a listed durable has another value, or when none of them exists.
A durable that does not exist is listed as not found without failing the check:
a `Dynamic` consumer creates a durable only while its partition is assigned.
Compare the durables not found with the current assignments;
one whose partition is assigned means that an instance could not create it.

Also check that the application logs contain no `max waiting can not be updated`.

---

## Manual procedure with the `nats` CLI

Steps 1 and 4 are the same as above.
Steps 2, 3 and 5 can be done with the [`nats` CLI](https://github.com/nats-io/natscli) and `jq`
when `partictl` is not available.
Nothing stops you here when a check fails, so read each result before going on.
The commands were run against nats-server v2.14.3 with `nats` CLI v0.3.1.

```sh
export NATS_URL=nats://nats.example:4222
STREAM=orders
```

### Before Step 1 — Check the stream

```sh
nats stream info "$STREAM" --json |
  jq '.config | {retention, max_age, max_msgs, max_bytes, discard}'
```

Compare `retention` with [the retention table](#1-the-streams-retention-policy-decides-what-a-recreated-durable-receives)
and plan the stop against `max_age`, which is shown in nanoseconds; 0 means none.

### Manual Step 2 — List the durables and record their state

Write the names of the durables to a file.

`Dynamic` creates one durable per partition, named `<ConsumerPrefix>_<partition>_<16 hex digits>`:

```sh
PREFIX=orders-consumer
nats consumer ls "$STREAM" -n | grep -E "^${PREFIX}_.+_[0-9a-f]{16}$" > durables.txt
```

`Queue` and `Static` use the consumer name you configured:

```sh
echo "my-queue-consumer" > durables.txt
```

Record the state of each one:

```sh
while read -r c; do
  nats consumer info "$STREAM" "$c" --json |
    jq -c '{name, filter: .config.filter_subject, max_waiting: .config.max_waiting,
            num_waiting, num_ack_pending, num_pending}'
done < durables.txt | tee durables-before.jsonl
```

Check three things before going on:

1. **Every line is a durable you mean to delete.**
   The name pattern also matches the durables of a longer prefix that starts the same way,
   such as `orders-consumer_eu`.
   The `filter` column shows the subject each one reads.
   Remove from `durables.txt` any line that does not belong.
2. **No durable has a pull request waiting.**
   This must print nothing:

   ```sh
   jq -c 'select(.num_waiting != 0)' durables-before.jsonl
   ```

   A durable listed here has a pull request waiting, so an instance is still running.
   Find and stop it, then record the state again.
   An empty result does not prove the opposite:
   an instance that is busy with a batch, reconnecting, or not pulling has no pull request waiting.
   The pod check in Step 1 is what establishes that everything is stopped.
3. **Which messages will be delivered a second time.**
   This lists the durables with messages that were delivered but not acknowledged:

   ```sh
   jq -c 'select(.num_ack_pending != 0)' durables-before.jsonl
   ```

   On a WorkQueue stream, messages that used up `MaxDeliver` are in no counter of the durable.
   They are what is left of the stream after the two counters are subtracted:

   ```sh
   nats stream info "$STREAM" --json | jq .state.messages
   jq -s 'map(.num_pending + .num_ack_pending) | add' durables-before.jsonl
   ```

   Take both readings while the publishers are quiet, or treat the difference as an upper bound:
   every message published between the two readings adds to it.
   The comparison holds when the listed durables cover every subject of the stream;
   otherwise compare per subject with `nats stream subjects "$STREAM"`.

Keep `durables-before.jsonl`.
It is the only record of the old state once the durables are gone.

### Manual Step 3 — Delete the durables

This is the point of no return: the positions cannot be restored.

```sh
while read -r c; do nats consumer rm "$STREAM" "$c" -f; done < durables.txt
```

If the loop is interrupted, run it again;
an error for a durable that is already gone is expected.
Confirm that none is left:

```sh
nats consumer ls "$STREAM" -n | grep -F -x -f durables.txt     # must print nothing
```

### Manual Step 5 — Verify

```sh
while read -r c; do
  if info=$(nats consumer info "$STREAM" "$c" --json 2>&1); then
    echo "$info" | jq -c '{name, max_waiting: .config.max_waiting, num_waiting, num_pending}'
  else
    echo "$c: $info"
  fi
done < durables.txt
```

- `max_waiting` shows the new value on every durable.
- A line with `consumer not found` is a durable that was not created again.
  For a `Dynamic` consumer that is expected only when its partition is not assigned right now;
  compare with the current assignments.
- Any other error means the check did not run for that durable.
- `num_pending` falls as the backlog drains.
- The application logs contain no `max waiting can not be updated`.

---

## If something goes wrong

**Instances report `max waiting can not be updated` after the start.**
A durable with a different value exists.
Either it was not in the list deleted in Step 3, or something created it between Step 3 and Step 4:
an instance with the old value, or `partictl consumers apply`.
Stop the instances again and repeat Steps 2 to 4.

**The command stops with `consumer is offline`, or cannot list the consumers.**
A NATS server that hosts one of the durables is down, so the durable can be neither read nor deleted.
When this happens during the checks, nothing was deleted.
When it happens during the delete itself, the result line says how many durables were deleted before it stopped.
Restore the NATS cluster first, then run the step again; durables that are already gone are skipped.

**The new application version has to be rolled back.**
Roll back the version and keep the new `MaxWaiting`.
The value belongs to the durables, not to the version:
an instance configured with the old value cannot bind the recreated durables.
Going back to the old value means running this procedure again.

**The procedure was interrupted after Step 3.**
Nothing needs repair.
Durables that are missing are created by the instances when they start;
continue with Step 4.
