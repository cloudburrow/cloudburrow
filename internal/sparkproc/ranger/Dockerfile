# syntax=docker/dockerfile:1
# The Dataproc RANGER optional component's Admin server (#2162), built on
# this machine from the pinned Apache Ranger 2.8.0 source release, as
# image version 3.0 runs it (2.2 and 2.3 run 2.4.0; the release notes,
# read 2026-11-23). Apache publishes no Ranger Admin image of its own
# release, so the image is built from source the way Ranger's own
# dev-support/ranger-docker builds it: a Maven build of the whole reactor
# (the distro's admin-web assembly needs every module), its
# ranger-2.8.0-admin.tar.gz unpacked into the runtime stage.
#
# Pins (dependencies.json's components.bigqueryml.dataprocRanger):
# - the source release by SHA-256 (Apache's published SHA-512 is
#   c00acf62...e9cf4; the SHA-256 is of the same verified file);
# - the build and runtime base, the Docker official maven image
#   3.9.11-eclipse-temurin-8 (Ranger 2.8 builds with Java 8), by its
#   multi-arch index digest;
# - MySQL Connector/J 8.0.28 (Ranger's own pin) from Maven Central by
#   SHA-256: GPL-2.0 with the Universal FOSS Exception, downloaded into
#   this locally built image only, never into the repository or the CLI.
# Maven resolves the build's own dependencies from Maven Central with
# checksum checking strict (-C), and the web UI's Node and npm packages
# through the frontend-maven-plugin, as Ranger's build does.
FROM maven@sha256:e3c149f44c95b0e9dd131862b3df67b3f061f7f6f3898a87b170564b3a943611 AS build
ADD --checksum=sha256:069ed1550160ac0da87980411c346d1e607466cb823720c73458a7b3b95b34aa \
    https://archive.apache.org/dist/ranger/2.8.0/apache-ranger-2.8.0.tar.gz /src/apache-ranger-2.8.0.tar.gz
RUN apt-get update && apt-get install -y --no-install-recommends python3 git && rm -rf /var/lib/apt/lists/* && \
    tar -xzf /src/apache-ranger-2.8.0.tar.gz -C /src && rm /src/apache-ranger-2.8.0.tar.gz
WORKDIR /src/apache-ranger-2.8.0
RUN mvn -B -C -q -DskipTests -DskipDocs -Dmaven.javadoc.skip=true -Drat.skip=true package && \
    ls -la target/ranger-2.8.0-admin.tar.gz && \
    mkdir /out && cp target/ranger-2.8.0-admin.tar.gz /out/ && rm -rf /root/.m2 /src

FROM maven@sha256:e3c149f44c95b0e9dd131862b3df67b3f061f7f6f3898a87b170564b3a943611
RUN apt-get update && apt-get install -y --no-install-recommends python3 procps bc && rm -rf /var/lib/apt/lists/* && \
    mkdir -p /opt/ranger /usr/share/java /var/log/ranger /var/run/ranger
ADD --checksum=sha256:a00ccdf537ff50e50067b989108c2235197ffb65e197149bbb669db843cd1c3e \
    https://repo1.maven.org/maven2/mysql/mysql-connector-java/8.0.28/mysql-connector-java-8.0.28.jar /usr/share/java/mysql-connector.jar
COPY --from=build /out/ranger-2.8.0-admin.tar.gz /tmp/
RUN tar -xzf /tmp/ranger-2.8.0-admin.tar.gz -C /opt/ranger && rm /tmp/ranger-2.8.0-admin.tar.gz && \
    ln -s /opt/ranger/ranger-2.8.0-admin /opt/ranger/admin
COPY ranger.sh /opt/cloudburrow/ranger.sh
EXPOSE 6080
ENTRYPOINT ["bash", "/opt/cloudburrow/ranger.sh"]
