load(":analyzers.bzl", _ANALYZER_NAMES = "ANALYZER_NAMES")

def _name_to_target(name):
    return Label("//build/analysis/staticcheck/analyzer:" + name)

def filtered_analyzers(filter):
    """Returns a list of all analyzers without the ones provided as list."""
    filtered = {
        name: _name_to_target(name)
        for name in _ANALYZER_NAMES
    }
    for name in filter:
        filtered.pop(name)
    return filtered.values()

ANALYZER_NAMES = _ANALYZER_NAMES

ANALYZER_TARGETS = [
    _name_to_target(name)
    for name in _ANALYZER_NAMES
]
