# Build context: this service directory.
FROM eclipse-temurin:25-jdk-alpine AS build
WORKDIR /src
COPY .mvn .mvn
COPY mvnw pom.xml ./
# Dependencies first, so they are cached until pom.xml changes.
RUN sh ./mvnw -B -q dependency:go-offline
COPY src src
RUN sh ./mvnw -B -q -DskipTests package && mv target/*.jar /app.jar

# OpenTelemetry Java agent: traces HTTP, JDBC and the JDK HttpClient without code changes. It is only attached
# when JAVA_TOOL_OPTIONS=-javaagent:/otel/agent.jar is set (compose does that), so benchmarks can run without it.
FROM alpine:3.24 AS otel
RUN wget -q -O /agent.jar https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases/latest/download/opentelemetry-javaagent.jar

FROM eclipse-temurin:25-jre-alpine
RUN adduser -D -u 10001 app
COPY --from=build /app.jar /app/app.jar
COPY --from=otel /agent.jar /otel/agent.jar
USER 10001
ENV PORT=8080
EXPOSE 8080
# Size the heap from the container memory limit (important when benchmarking with --memory).
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75", "-jar", "/app/app.jar"]
