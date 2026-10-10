package io.projectbooth.spark;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.Map;

import org.apache.iceberg.rest.HTTPHeaders;
import org.apache.iceberg.rest.HTTPRequest;
import org.apache.iceberg.rest.RESTClient;
import org.apache.iceberg.rest.auth.AuthManager;
import org.apache.iceberg.rest.auth.AuthSession;
import org.apache.iceberg.rest.auth.DefaultAuthSession;

/**
 * Iceberg REST authentication with the run's workload token (docs/design-v0.md item 4).
 *
 * <p>The run's agent keeps the token current in a file, re-minted every few minutes. Iceberg's own
 * {@code token} property is read once, so a run would outlive it; this manager reads the file on
 * every REST call instead. Set {@code rest.auth.type} to this class and {@code booth.token-file} to
 * the file.
 */
public final class TokenFileAuthManager implements AuthManager {
    /** Catalog property naming the token file. */
    public static final String TOKEN_FILE = "booth.token-file";

    public TokenFileAuthManager() {}

    public TokenFileAuthManager(String name) {}

    @Override
    public AuthSession catalogSession(RESTClient sharedClient, Map<String, String> properties) {
        String f = properties.get(TOKEN_FILE);
        if (f == null || f.isBlank()) {
            throw new IllegalArgumentException("booth-spark: catalog property " + TOKEN_FILE + " is not set");
        }
        return new Session(Paths.get(f));
    }

    @Override
    public void close() {}

    static final class Session implements AuthSession {
        private final Path file;

        Session(Path file) {
            this.file = file;
        }

        @Override
        public HTTPRequest authenticate(HTTPRequest request) {
            String token;
            try {
                token = Files.readString(file, StandardCharsets.UTF_8).trim();
            } catch (IOException e) {
                throw new UncheckedIOException("booth-spark: reading the run's token from " + file, e);
            }
            HTTPHeaders headers = HTTPHeaders.of(HTTPHeaders.HTTPHeader.of("Authorization", "Bearer " + token));
            return DefaultAuthSession.of(headers).authenticate(request);
        }

        @Override
        public void close() {}
    }
}
