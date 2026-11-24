# The template's setup file (FLEX_TEMPLATE_PYTHON_SETUP_FILE): its package
# is built into a source distribution by the SDK at launch and installed
# on the job's workers.
import setuptools

setuptools.setup(name="cbsetup", version="0.1.0", packages=["cbsetup"])
