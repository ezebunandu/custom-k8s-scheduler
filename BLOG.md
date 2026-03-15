# Blog Post: Teaching Kubernetes Where to Put Things

**Working title:** *"Teaching Kubernetes Where to Put Things: Writing a Custom Scheduler From Scratch"*

---

## Structure & Points to Cover

### 1. Open with the mystery, not the definition
Don't open with "Kubernetes has a scheduler." Instead, open with the question a beginner naturally asks: *"When I run a pod, how does Kubernetes decide which machine it goes to?"* Let that curiosity be the thread.

---

### 2. The postal sorting analogy for the default scheduler
The default scheduler is like a postal worker who knows every address in the city and has rules for how to distribute parcels efficiently (node resources, affinity, taints). Most of the time you don't think about it — it just works. But what if you wanted parcels sorted *alphabetically by street name* instead?

---

### 3. What a scheduler actually does — the three-step loop
Keep it concrete, no jargon dump:
- **Filter** — which nodes *can't* take this pod? (no room, wrong zone, etc.)
- **Score** — of the nodes that can, which one *should* we prefer?
- **Bind** — tell Kubernetes: this pod goes here

Your project only touches **Score**, which is a natural scoping decision to explain — beginners appreciate a bounded problem.

---

### 4. The plugin framework: "Kubernetes lets you swap out parts"
Introduce the scheduler plugin interface as a way Kubernetes exposes extension points. You only have to implement `Score()` and `ScoreExtensions()` — the rest of the machinery keeps running. A nice analogy: like installing a custom sort function into an otherwise standard sorting algorithm.

---

### 5. The TDD angle — tests first, code second
This is the demo project's most interesting angle for a beginner. Show the failing test output *before* the implementation. The point to make: **the test describes what the code should do, not what it currently does.** This is a concept beginners often find counterintuitive and worth demystifying.

Show the table:
- `"alpha"` → 100
- `"bravo"` → 96
- missing label → 0

Then show how those become test cases, then how implementation makes them green. The numbers are simple enough that the scoring formula feels satisfying, not intimidating.

---

### 6. The mock-handle trick (the "interesting plumbing" moment)
Briefly explain *why* you use interface embedding for the fake handle — "rather than spinning up a real Kubernetes cluster just to test one function, we fake just the part we need." This is a transferable testing concept your readers will carry into other projects.

---

### 7. Kind E2E — "let's torture Kubernetes to confirm"
Spin up a 4-node cluster, label the nodes, watch the pod land on the `alpha` node. Show a simple `kubectl get pod -o wide` proving the pod went to the right node. Readers will love seeing the real cluster output.

---

### 8. The design tradeoff you made: framework vs. standalone binary
You pivoted from the full `app.NewSchedulerCommand` (heavy k8s internal dependency) to a simpler polling binary using only `client-go`. That decision is worth a short honest paragraph — "here's what I tried, here's why it didn't work cleanly, here's what I did instead." Beginners learn as much from the wrong turns as the right ones.

---

### 9. Close with "So what does this actually unlock?"
- Custom scheduling for cost (schedule batch jobs to spot nodes alphabetically by cheapness label)
- Topology-aware placement (schedule by region/zone labels)
- Compliance (certain workloads must go to certain nodes)

The alphabetical example is a toy, but the *mechanism* is real — that's the "aha" moment worth ending on.

---

## Structural Note
Given your style, avoid splitting this into two posts. The TDD-to-E2E arc is the story — cutting it at the unit test level loses the payoff of watching it work in a real cluster.
