package es.tracklin;

import java.util.Map;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.context.embedded.LocalServerPort;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.client.TestRestTemplate;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.test.context.junit4.SpringRunner;

import static org.assertj.core.api.BDDAssertions.then;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class, webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT)
public class TrackTests {
    @LocalServerPort
    private int port;

    @Autowired
    private TestRestTemplate testRestTemplate;

    @Test
    public void shouldReturn200WhenSendingRequestToTrackController() throws Exception {
        @SuppressWarnings("rawtypes")
        ResponseEntity<Map> entity = this.testRestTemplate.getForEntity(
                "http://localhost:" + this.port + "/v1/track",
                Map.class
        );
        then(entity.getStatusCode()).isEqualTo(HttpStatus.OK);
    }
}