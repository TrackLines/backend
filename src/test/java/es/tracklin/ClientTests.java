package es.tracklin;

import es.tracklin.Client.LoginModel;
import es.tracklin.Client.RegisterModel;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.context.embedded.LocalServerPort;
import org.springframework.boot.test.autoconfigure.web.client.AutoConfigureWebClient;
import org.springframework.boot.test.autoconfigure.web.servlet.AutoConfigureMockMvc;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.client.TestRestTemplate;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.test.context.junit4.SpringRunner;

import java.util.Map;

import static org.assertj.core.api.BDDAssertions.then;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class, webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT)
public class ClientTests {
    @LocalServerPort
    private int port;

    @Autowired
    private TestRestTemplate testRestTemplate;

    @Test
    public void shouldReturn401WhenSendingRequestToClientController() throws Exception {
        @SuppressWarnings("rawtypes")
        ResponseEntity<Map> entity = this.testRestTemplate.getForEntity(
                "http://localhost:" + this.port + "/v1/client",
                Map.class
        );
        then(entity.getStatusCode()).isEqualTo(HttpStatus.UNAUTHORIZED);
    }

    @Test
    public void shouldReturn401WhenSendingWrongLoginRequest() throws Exception {
        LoginModel loginModel = new LoginModel();
        loginModel.setUsername("bob");
        loginModel.setPassword("bill");

        @SuppressWarnings("rawtypes")
        ResponseEntity<Map> entity = this.testRestTemplate.postForEntity(
                "http://localhost:" + this.port + "/v1/client",
                loginModel,
                Map.class
        );
        then(entity.getStatusCode()).isEqualTo(HttpStatus.UNAUTHORIZED);
    }

    @Test
    public void shouldReturn200WhenSendingRequestToRegisterAndThenLogin() throws Exception {
        RegisterModel registerModel = new RegisterModel();
        registerModel.setUsername("bobby");
        registerModel.setPassword("bobster");
        registerModel.setCompanyName("test");
        registerModel.setContactEmail("bob@test.com");

        this.testRestTemplate.put(
                "http://localhost:" + this.port + "/v1/client",
                registerModel
        );

        LoginModel loginModel = new LoginModel();
        loginModel.setUsername("bobby");
        loginModel.setPassword("bobster");

        @SuppressWarnings("rawtypes")
        ResponseEntity<Map> entity = this.testRestTemplate.postForEntity(
                "http://localhost:" + this.port + "/v1/client",
                loginModel,
                Map.class
        );
        then(entity.getStatusCode()).isEqualTo(HttpStatus.OK);
    }

    @Test
    public void shouldReturn401WhenSendingRequestToDeleteAccount() throws Exception {

    }
}