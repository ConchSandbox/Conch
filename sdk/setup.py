from setuptools import setup, find_packages

setup(
    name="Conch",
    version="0.1.0",
    packages=find_packages(),
    install_requires=[
        "requests>=2.25.0",
        "grpcio>=1.50.0",
        "protobuf>=4.0.0",
        "PyYAML>=5.4.0",
    ],
    python_requires=">=3.8",
    description="SDK for conch service",
    url="https://atomgit.com/openeuler/Conch"
);