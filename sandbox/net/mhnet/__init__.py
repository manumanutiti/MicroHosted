# The sandbox's network, inside the VM: a resolver and a sinkhole that
# answer as the internet would. Nothing leaves the VM; what the code meant
# to reach is written down. The entry point is mh-sandbox-net (main.py);
# docs/sandbox.md, "Its network", says what it does.
#
# Everything here parses what the code chose to send: it runs as mhsink,
# neither root nor the code, from a directory only root can write, and
# every reading of the code's bytes is bounded — in size, in time, in work.
