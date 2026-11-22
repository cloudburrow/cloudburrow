#!/bin/sh
# CloudBurrow Model Garden serving runtime (#2148), Apache-2.0.
#
# Runs in the pinned Python image: installs the runtime's pinned packages
# (requirements.txt, from PyPI and PyTorch's CPU index, cached on the node)
# into a virtual environment, downloads the model from Hugging Face at its
# pinned revision when the runtime does not do so itself, and starts the
# server on $AIP_HTTP_PORT. RUNTIME names the runtime: hf-pytorch, mlx or
# onnx.
set -eu
export PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_INPUT=1 PIP_ROOT_USER_ACTION=ignore PIP_CACHE_DIR=/cache/pip
echo "cloudburrow: installing the ${RUNTIME} serving runtime"
python -m venv /opt/runtime
. /opt/runtime/bin/activate
pip install --progress-bar off -r /etc/cloudburrow/requirements.txt
if [ -s /etc/cloudburrow/requirements-nodeps.txt ]; then
	pip install --progress-bar off --no-deps -r /etc/cloudburrow/requirements-nodeps.txt
fi
export PYTHONPATH=/etc/cloudburrow
case "$RUNTIME" in
hf-pytorch)
	# Google's Hugging Face PyTorch Inference container's server
	# (huggingface-inference-toolkit), which downloads HF_MODEL_ID itself.
	exec uvicorn cloudburrow_hf:app --host 0.0.0.0 --port "$AIP_HTTP_PORT" --no-access-log
	;;
mlx)
	python /etc/cloudburrow/fetch.py /data/model
	exec python -m mlx_lm.server --model /data/model --host 0.0.0.0 --port "$AIP_HTTP_PORT"
	;;
onnx)
	python /etc/cloudburrow/fetch.py /data/model
	exec python /etc/cloudburrow/onnx_server.py /data/model
	;;
esac
echo "cloudburrow: unknown runtime ${RUNTIME}" >&2
exit 2
