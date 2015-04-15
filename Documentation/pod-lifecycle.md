# Life-cycle of a pod in rkt

Throughout this document `$var` is used to refer to the directory `/var/lib/rkt/pods`, and `$uuid` refers to a pod's UUID e.g. "076292e6-54c4-4cc8-9fa7-679c5f7dcfd3".

Due to rkt's lack of a management daemon process, a combination of advisory file locking and atomically changing (via rename(2)) directory locations is used to represent and transition the basic pod states.

At times where a state must be reliably coupled to an executing process, that process is executed with an open file descriptor possessing an exclusive advisory lock on the respective pod's directory.  Should that process exit for any reason, its open file descriptors will automatically be closed by the kernel, implicitly unlocking the pod's directory in the process.  By attempting to acquire a shared non-blocking advisory lock on a pod directory we're able to poll for these process-bound states, additionally by employing a blocking acquisition mode we may reliably synchronize indirectly with the exit of such processes, effectively providing us with a wake-up event the moment such a state transitions.  For more information on advisory locks see the flock(2) man page.

At this time there are four distinct phases of a pod's life which involve process-bound states:

* Prepare
* Run
* ExitedGarbage
* Garbage

Each of these phases involves an exclusive lock on a given pod's directory.  As an exclusive lock by itself cannot express both the phase and process-bound activity within that phase, we combine the lock with the pod's directory location to represent the whole picture:

| Phase         | Directory                   | Locked exclusively      | Unlocked                 |
|---------------|-----------------------------|-------------------------|--------------------------|
| Prepare       | "$var/prepare/$uuid"        | preparing               | prepare-failed           |
| Run           | "$var/run/$uuid"            | running                 | exited                   |
| ExitedGarbage | "$var/exited-garbage/$uuid" | exited+deleting         | exited+gc-marked         |
| Garbage       | "$var/garbage/$uuid"        | prepare-failed+deleting | prepare-failed+gc-marked |

To prevent the period between first creating a pod's directory and acquiring its lock from appearing as prepare-failed in the Prepare phase, and to provide a phase for prepared pods where they may dwell and the lock may be acquired prior to entering the Run phase, two additional directories are employed where locks have no meaning:

| Phase           | Directory                   | Locked exclusively      | Unlocked                 |
|-----------------|-----------------------------|-------------------------|--------------------------|
| Embryo          | "$var/embryo/$uuid"         | -                       | -                        |
| Prepare         | "$var/prepare/$uuid"        | preparing               | prepare-failed           |
| Prepared        | "$var/prepared/$uuid"       | -                       | -                        |
| Run             | "$var/run/$uuid"            | running                 | exited                   |
| ExitedGarbage   | "$var/exited-garbage/$uuid" | exited+deleting         | exited+gc-marked         |
| Prepare-failed  | "$var/garbage/$uuid"        | prepare-failed+deleting | prepare-failed+gc-marked |

These phases, their function, and how they proceed through their respective states is explained in more detail below.

## Embryo

`rkt run` and `rkt prepare` instantiate a new pod by creating an empty directory at `$var/embryo/$uuid`.

An exclusive lock is immediately acquired on the created directory which is then renamed to `$var/prepare/$uuid`, transitioning to the #Prepare phase.

## Prepare

`rkt run` and `rkt prepare` enter this phase identically; holding an exclusive lock on the pod directory `$var/prepare/$uuid`.

After preparation completes, while still holding the exclusive lock (the lock is held for the duration):

`rkt prepare` transitions to #Prepared by renaming `$var/prepare/$uuid` to `$var/prepared/$uuid`.

`rkt run` transitions directly from #Prepare to #Run by renaming `$var/prepare/$uuid` to `$var/run/$uuid`, entirely skipping the #Prepared phase.

Should #Prepare fail or be interrupted, `$var/prepare/$uuid` will be left in an unlocked state.  Any directory in `$var/prepare` in an unlocked state is considered a failed prepare.  `rkt gc` identifies failed prepares in need of cleanup by trying to acquire a shared lock on all directories in `$var/prepare`, renaming successfully locked directories to `$var/garbage` where they are then deleted.

## Prepared

`rkt prepare` concludes successfully by leaving the pod directory at `$var/prepared/$uuid` in an unlocked state before returning $uuid to the user.

`rkt run-prepared` resumes where `rkt prepare` concluded by exclusively locking the pod at `$var/prepared/$uuid` before renaming it to `$var/run/$uuid`, specifically acquiring the lock prior to entering the #Run phase.

`rkt run` never enters this phase, skipping directly from #Prepare to #Run with the lock held.

## Run

`rkt run` and `rkt run-prepared` both arrive here with the pod at `$var/run/$uuid` while holding the exclusive lock.

The pod is then excuted while holding this lock.  It is required that the stage 1 `coreos.com/rkt/stage1/run` entrypoint keep the file descriptor representing the exclusive lock open for the lifetime of the pod's process.  All this requires is that the stage 1 implementation not close the inherited file descriptor.  This is facilitated by supplying stage 1 its number in the RKT_LOCK_FD environment variable.

What follows applies equally to `rkt run` and `rkt run-prepared`.

## Death / exit

A pod is considered exited if a shared lock can be acquired on `$var/run/$uuid`.  Upon exit of a pod's process, the exclusive lock acquired before entering the #Run phase becomes released by the kernel.

## Garbage collection

Exited pods are discarded using a common mark-and-sweep style of garbage collection by invoking the `rkt gc` command.  This relatively simple approach lends itself well to a minimal file-system based implementation utilizing no additional daemons or record keeping with good efficiency.  The process is performed in two distinct passes explained in detail below.

### Pass 1: mark

All directories found in `$var/run` are tested for exited status by trying to acquire a shared advisory lock on each directory.

When a directory's lock cannot be acquired, the directory is skipped as it indicates the pod is currently executing.

For directories which the lock is successfully acquired, the directory is renamed from `$var/run/$uuid` to `$var/exited-garbage/$uuid`.  The renaming effectively implements the "mark" operation.  The locks are immediately released, and operations like `rkt status` may occur simultaneous to `rkt gc`.

Marked exited pods dwell in the `$var/exited-garbage` directory for a grace period during which their status may continue to be queried by `rkt status`.  The rename from `$var/run/$uuid` to `$var/exited-garbage/$uuid` serves in part to keep marked pods from cluttering the `$var/run` directory during their respective dwell periods.

### Pass 2: sweep

A side-effect of the rename operation responsible for moving a pod from `$var/run` to `$var/exited-garbage` is an update to the pod directory's change time.  The sweep operation takes advantage of this in honoring the necessary grace period before discarding exited pods.  This grace period currently defaults to 30 minutes, and may be explicitly specified using the `--grace-period duration` flag with `rkt gc`.  Note that this grace period begins from the time a pod was marked by `rkt gc`, not when the pod exited.  A pod becomes eligible for marking upon exit, but will not become marked until a subsequent `rkt gc` is performed.

The change times of all directories found in `$var/exited-garbage` are compared against the current time.  Directories having sufficiently old change times are locked exclusively and recursively deleted.  If a lock acquisition fails, the directory is skipped.  Failed exclusive lock acquisitions may occur if the garbage pod is currently being accessed via `rkt status`, or deleted by a concurrent `rkt gc`, for example.  The skipped pods will be revisited on a subsequent `rkt gc` invocation's sweep pass.

## Pulse

To answer the questions "Has this pod exited?" and "Is this pod being deleted?" the pod's UUID is looked for in `$var/run` and `$var/exited-garbage`, respectively.  Pods found in the `$var/exited-garbage` directory must already be exited, and a shared lock acquisition may be used to determine if the garbage pod is actively being deleted.  Those found in the `$var/run` directory may be exited or running, a failed shared lock acquisition indicates a pod in `$var/run` is alive at the time of the failed acquisition.

Care must be taken when acting on what is effectively always going to be stale knowledge of pod state; though a pod's status may be found to be "running" by the mechanisms documented here, this was an instantaneously sampled state that was true at the time sampled (failed lock attempt at $var/run/$uuid), and may cease to be true by the time code execution progressed to acting on that sample.  Pod exit is totally asynchronous and cannot be prevented, relevant code must take this into consideration (e.g. `rkt enter`) and be tolerant of states progressing.

For example, two `rkt run-prepared` invocations for the same UUID may occur simultaneously.  Only one of these will successfully transition the pod from #Prepared to #Run due to rename's atomicity, which is exactly what we want.  The loser of this race needs to simply inform the user of the inability to transition the pod to the run state, perhaps with a check to see if the pod transitioned independently and a useful message mentioning it.

Another example would be two `rkt gc` commands finding the same exited pods and attempting to transition them to the #Garbage phase concurrently.  They can't both perform the transitions, one will lose the race at each same pod.  This needs to be considered in the error handling of the transition callers as perfectly normal, simply ignoring ENOENT errors propagated from the loser's rename calls can suffice.
