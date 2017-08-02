/*
 * MIT License
 *
 * Copyright (c) 2017 TrackLin.es
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package es.tracklin;

import es.tracklin.Client.LoginModel;
import es.tracklin.Client.RegisterModel;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.context.embedded.LocalServerPort;
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
    public void shouldReturn403WhenSendingWrongLoginRequest() throws Exception {
        LoginModel loginModel = new LoginModel();
        loginModel.setUsername("bob");
        loginModel.setPassword("bill");

        @SuppressWarnings("rawtypes")
        ResponseEntity<Map> entity = this.testRestTemplate.postForEntity(
                "http://localhost:" + this.port + "/v1/client",
                loginModel,
                Map.class
        );
        then(entity.getStatusCode()).isEqualTo(HttpStatus.FORBIDDEN);
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