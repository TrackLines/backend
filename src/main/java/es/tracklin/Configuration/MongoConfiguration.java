package es.tracklin.Configuration;

import org.hibernate.validator.constraints.Length;
import org.hibernate.validator.constraints.NotBlank;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.context.annotation.Configuration;
import org.springframework.context.annotation.PropertySource;
import org.springframework.stereotype.Component;
import org.springframework.validation.annotation.Validated;

import javax.validation.constraints.Max;
import javax.validation.constraints.Min;

@Configuration
@PropertySource("classpath:mongo.properties")
@PropertySource(value = "classpath:local.properties", ignoreResourceNotFound = true)
@ConfigurationProperties(prefix = "mongo")
@Validated
public class MongoConfiguration {
    @Validated
    public static class Credentials {
        @Length(max = 40, min = 4)
        private String username;

        @Length(max = 40, min = 8)
        private String password;

        public String getUsername() {
            return username;
        }

        public void setUsername(String username) {
            this.username = username;
        }

        public String getPassword() {
            return password;
        }

        public void setPassword(String password) {
            this.password = password;
        }
    }

    private Credentials credentials;

    @NotBlank
    private String address;

    @Min(27000)
    @Max(28000)
    private int port;

    public Credentials getCredentials() {
        return credentials;
    }

    public void setCredentials(Credentials credentials) {
        this.credentials = credentials;
    }

    public String getAddress() {
        return address;
    }

    public void setAddress(String address) {
        this.address = address;
    }

    public int getPort() {
        return port;
    }

    public void setPort(int port) {
        this.port = port;
    }
}
