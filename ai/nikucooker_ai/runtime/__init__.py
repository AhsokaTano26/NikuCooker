"""Runtime introspection: what this host can do, and whether the environment is
sound enough to attempt it.

Nothing here loads a model. The self-check exists so the core can find out what
is missing *before* spawning a worker for real work, and report it with a
remediation the user can act on.
"""
