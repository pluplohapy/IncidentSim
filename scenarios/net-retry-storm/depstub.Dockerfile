# Dedicated image for the `dep` stack's depstub service.
#
# The shared incidentsim/workload:dev image carries the unmodified
# incidentsim-depstub binary but its ENTRYPOINT is the legacy workload
# service, so a compose `command` cannot select the stub. Layering a fixed
# ENTRYPOINT over that image reuses the exact binary without touching the
# shared image or the stub source.
FROM incidentsim/workload:dev
ENTRYPOINT ["/usr/local/bin/incidentsim-depstub"]
