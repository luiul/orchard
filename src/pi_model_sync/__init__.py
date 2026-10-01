"""pi-model-sync package root.

`__version__` is resolved lazily (PEP 562 module `__getattr__`) instead of
eagerly at import time: `importlib.metadata.version()` scans every installed
distribution's metadata, a tax on every CLI invocation for an attribute
nothing on the command path reads.
"""

__all__ = ["__version__"]


def __getattr__(name: str) -> str:
    if name == "__version__":
        from importlib.metadata import PackageNotFoundError, version

        try:
            return version("pi-model-sync")
        except PackageNotFoundError:
            return "0.0.0+local"
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
